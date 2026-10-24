use crate::{
    filesystem::PRIVATE,
    state::App,
    web::{ApiError, header, response},
};
use askama::Template;
use axum::{
    body::Body,
    http::{HeaderMap, StatusCode, Uri},
    response::Response,
};
use chrono::{DateTime, SecondsFormat};
use percent_encoding::{AsciiSet, CONTROLS, utf8_percent_encode};
use rustix::fs::OFlags;
use std::sync::Arc;

// Encode delimiters too: names must remain a single path segment in HTML/Markdown.
const SEGMENT: &AsciiSet = &CONTROLS
    .add(b' ')
    .add(b'"')
    .add(b'#')
    .add(b'%')
    .add(b'/')
    .add(b'<')
    .add(b'>')
    .add(b'?')
    .add(b'`')
    .add(b'{')
    .add(b'}')
    .add(b'\\')
    .add(b'[')
    .add(b']')
    .add(b'(')
    .add(b')')
    .add(b'\'')
    .add(b'|');
pub fn encode_segment(s: &str) -> String {
    utf8_percent_encode(s, SEGMENT).to_string()
}
pub fn encode_path(s: &str) -> String {
    s.split('/')
        .map(encode_segment)
        .collect::<Vec<_>>()
        .join("/")
}
pub fn js_string(s: &str) -> String {
    serde_json::to_string(s)
        .unwrap()
        .replace('<', "\\u003c")
        .replace('>', "\\u003e")
        .replace('&', "\\u0026")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
}
pub struct FileInfo {
    pub name: String,
    pub size: String,
    pub last_modified: String,
    pub is_dir: bool,
    pub url: String,
    pub size_bytes: u64,
    pub mod_unix: i64,
    pub expires_unix: i64,
}
pub struct Crumb {
    pub label: String,
    pub url: String,
}
#[derive(Template)]
#[template(path = "index.html")]
struct Page<'a> {
    title: &'a str,
    current_path: &'a str,
    extra_headers: &'a str,
    disable_upload: bool,
    parent_url: String,
    git_commit: &'a str,
    build_date: &'a str,
    curl_upload_example: String,
    breadcrumbs: Vec<Crumb>,
    files: Vec<FileInfo>,
    current_path_js: String,
}
pub fn format_size(bytes: u64) -> String {
    if bytes < 1024 {
        return format!("{bytes} B");
    }
    let mut value = bytes as f64 / 1024.;
    let mut i = 0;
    while value >= 1024. && i < 5 {
        value /= 1024.;
        i += 1;
    }
    format!("{value:.1} {}B", ["K", "M", "G", "T", "P", "E"][i])
}
pub fn wants_markdown(headers: &HeaderMap) -> bool {
    let agent = header(headers, "user-agent").trim().to_ascii_lowercase();
    if agent == "curl" || agent.starts_with("curl/") {
        return true;
    }
    let mut md = (-1, 0.0_f64);
    let mut html = (-1, 0.0_f64);
    let mut explicit = false;
    for value in headers.get_all("accept") {
        let Ok(value) = value.to_str() else { continue };
        for part in value.split(',') {
            let mut parts = part.split(';');
            let media = parts.next().unwrap_or("").trim().to_ascii_lowercase();
            let mut quality = 1.0_f64;
            let mut valid = true;
            for parameter in parts {
                if let Some((key, value)) = parameter.trim().split_once('=')
                    && key.eq_ignore_ascii_case("q")
                {
                    quality = value.trim().trim_matches('"').parse().unwrap_or(f64::NAN);
                    valid = (0.0..=1.0).contains(&quality);
                }
            }
            if !valid {
                continue;
            }
            let update = |pref: &mut (i32, f64), specificity| {
                if specificity > pref.0 || (specificity == pref.0 && quality > pref.1) {
                    *pref = (specificity, quality);
                }
            };
            match media.as_str() {
                "text/markdown" => {
                    explicit = true;
                    update(&mut md, 2);
                }
                "text/html" => update(&mut html, 2),
                "text/*" => {
                    update(&mut md, 1);
                    update(&mut html, 1);
                }
                "*/*" => {
                    update(&mut md, 0);
                    update(&mut html, 0);
                }
                _ => (),
            }
        }
    }
    explicit && md.1 > 0.0 && md.1 >= html.1
}
fn markdown_text(text: &str) -> String {
    let mut out = String::new();
    for c in text.chars() {
        match c {
            c if c.is_control() || matches!(c, '\u{2028}' | '\u{2029}') => out.push(' '),
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&#34;"),
            '\'' => out.push_str("&#39;"),
            c => {
                if "\\`*_[](){}#!|~".contains(c) {
                    out.push('\\');
                }
                out.push(c);
            }
        }
    }
    out
}
fn absolute_url(headers: &HeaderMap, path: &str) -> String {
    let scheme = if header(headers, "x-forwarded-proto").eq_ignore_ascii_case("https") {
        "https"
    } else {
        "http"
    };
    format!(
        "{scheme}://{}{}",
        header(headers, "host"),
        encode_path(path)
    )
}
fn timestamp(unix: i64) -> String {
    DateTime::from_timestamp(unix, 0)
        .unwrap_or(DateTime::UNIX_EPOCH)
        .to_rfc3339_opts(SecondsFormat::Secs, true)
}
pub async fn directory(
    app: Arc<App>,
    uri: Uri,
    headers: HeaderMap,
    path: String,
    rel: String,
    head: bool,
) -> Result<Response, ApiError> {
    tokio::task::spawn_blocking(move || render_directory(&app, &uri, &headers, &path, &rel, head))
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?
}

fn render_directory(
    app: &App,
    uri: &Uri,
    headers: &HeaderMap,
    path: &str,
    rel: &str,
    head: bool,
) -> Result<Response, ApiError> {
    let mut files = Vec::new();
    let entries = app.files.names(rel)?;
    let expirations = app.actions.lock().unwrap().clone();
    for name in entries {
        if name == PRIVATE {
            continue;
        }
        let target = if rel == "." {
            name.clone()
        } else {
            format!("{rel}/{name}")
        };
        let Ok(file) = app.files.open_file(&target, OFlags::RDONLY, 0) else {
            continue;
        };
        let Ok(info) = file.metadata() else { continue };
        let modified = info.modified().unwrap_or(std::time::UNIX_EPOCH);
        let date: DateTime<chrono::Local> = modified.into();
        let (bytes, complete) = if info.is_dir() {
            let size = app.dir_size(&target, modified);
            (size.bytes, size.complete)
        } else {
            (info.len(), true)
        };
        files.push(FileInfo {
            url: format!(
                "./{}{}",
                encode_segment(&name),
                if info.is_dir() { "/" } else { "" }
            ),
            name,
            size: format!("{}{}", if complete { "" } else { "~" }, format_size(bytes)),
            last_modified: date.format("%Y-%m-%d %H:%M%:z").to_string(),
            is_dir: info.is_dir(),
            size_bytes: bytes,
            mod_unix: date.timestamp(),
            expires_unix: expirations
                .get(&target)
                .map_or(0, |e| e.expires_at.timestamp()),
        });
    }
    files.sort_by(|a, b| b.is_dir.cmp(&a.is_dir).then(a.name.cmp(&b.name)));
    let parent = if path == "/" {
        "/".into()
    } else {
        format!(
            "{}/",
            path.trim_end_matches('/')
                .rsplit_once('/')
                .map_or("", |(parent, _)| parent)
        )
    };
    let curl_example = format!(
        "curl -T file.txt '{}'",
        absolute_url(headers, &format!("{path}file.txt")).replace('\'', "'\"'\"'")
    );
    let markdown = wants_markdown(headers);
    let body = if markdown {
        let mut out = format!(
            "# {}\n\nDirectory: {}\n\n",
            markdown_text(&app.config.title),
            markdown_text(path)
        );
        if path != "/" {
            out += &format!(
                "[Parent directory](<{}>)\n\n",
                absolute_url(headers, &parent)
            );
        }
        if files.is_empty() {
            out += "This directory is empty.\n";
        } else {
            out += "| Name | Type | Size | Modified (UTC) | Expires (UTC) |\n| --- | --- | --- | --- | --- |\n";
            for file in &files {
                let suffix = if file.is_dir { "/" } else { "" };
                out += &format!(
                    "| [{}{}](<{}>) | {} | {} | {} | {} |\n",
                    markdown_text(&file.name),
                    suffix,
                    absolute_url(headers, &format!("{path}{}{suffix}", file.name)),
                    if file.is_dir { "directory" } else { "file" },
                    file.size,
                    timestamp(file.mod_unix),
                    if file.expires_unix > 0 {
                        timestamp(file.expires_unix)
                    } else {
                        "—".into()
                    }
                );
            }
        }
        if app.config.upload {
            out += &format!("\n## Upload a file\n\n```sh\n{curl_example}\n```\n");
        }
        out
    } else {
        let mut breadcrumbs = vec![Crumb {
            label: "/".into(),
            url: "/".into(),
        }];
        let mut current = String::from("/");
        for part in path.trim_matches('/').split('/').filter(|p| !p.is_empty()) {
            current += &format!("{}/", encode_segment(part));
            breadcrumbs.push(Crumb {
                label: format!("{part}/"),
                url: current.clone(),
            });
        }
        Page {
            title: &app.config.title,
            current_path: path,
            extra_headers: &app.config.extra_headers,
            disable_upload: !app.config.upload,
            parent_url: encode_path(&parent),
            git_commit: env!("GIT_COMMIT"),
            build_date: env!("BUILD_DATE"),
            curl_upload_example: curl_example,
            breadcrumbs,
            files,
            current_path_js: js_string(path),
        }
        .render()
        .map_err(|e| ApiError::internal(e.to_string()))?
    };
    let length = body.len();
    let mut result = response(
        StatusCode::OK,
        if head {
            Body::empty()
        } else {
            Body::from(body)
        },
    );
    let h = result.headers_mut();
    h.insert(
        "content-type",
        if markdown {
            "text/markdown; charset=utf-8"
        } else {
            "text/html; charset=utf-8"
        }
        .parse()
        .unwrap(),
    );
    h.insert("content-length", length.into());
    h.insert("vary", "Accept, User-Agent".parse().unwrap());
    h.insert(
        "link",
        format!(
            "<{}>; rel=\"alternate\"; type=\"text/markdown\"",
            uri.path()
        )
        .parse()
        .unwrap(),
    );
    if markdown {
        h.insert("x-content-type-options", "nosniff".parse().unwrap());
    }
    Ok(result)
}
