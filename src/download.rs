use crate::{
    filesystem::new_id,
    listing::encode_segment,
    web::{ApiError, header, response},
};
use axum::{
    body::{Body, Bytes},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use std::{
    fs::File,
    io,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::io::{AsyncReadExt, AsyncSeekExt};
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::io::ReaderStream;

fn disposition(name: &str, inline: bool) -> String {
    let name: String = name
        .chars()
        .filter(|c| !c.is_control() && *c != '/' && *c != '\\')
        .collect();
    let name = if name.is_empty() { "download" } else { &name };
    format!(
        "{}; filename=\"{}\"; filename*=UTF-8''{}",
        if inline { "inline" } else { "attachment" },
        name.replace('"', "\\\""),
        encode_segment(name)
    )
}
fn parse_ranges(value: &str, size: u64) -> Result<Vec<(u64, u64)>, ApiError> {
    let value = value
        .strip_prefix("bytes=")
        .ok_or_else(|| ApiError::new(StatusCode::RANGE_NOT_SATISFIABLE, "Invalid range"))?;
    let mut ranges = Vec::new();
    for part in value.split(',') {
        let (start, end) = part
            .trim()
            .split_once('-')
            .ok_or_else(|| ApiError::new(StatusCode::RANGE_NOT_SATISFIABLE, "Invalid range"))?;
        let invalid = || ApiError::new(StatusCode::RANGE_NOT_SATISFIABLE, "Invalid range");
        if start.is_empty() {
            let suffix: u64 = end.parse().map_err(|_| invalid())?;
            if suffix == 0 || size == 0 {
                continue;
            }
            ranges.push((size.saturating_sub(suffix), size - 1));
        } else {
            let start: u64 = start.parse().map_err(|_| invalid())?;
            if start >= size {
                continue;
            }
            let end = if end.is_empty() {
                size - 1
            } else {
                end.parse::<u64>().map_err(|_| invalid())?.min(size - 1)
            };
            if end < start {
                return Err(invalid());
            }
            ranges.push((start, end));
        }
        if ranges.len() > 100 {
            return Err(invalid());
        }
    }
    if ranges.is_empty() {
        return Err(ApiError::new(
            StatusCode::RANGE_NOT_SATISFIABLE,
            "Range does not overlap file",
        ));
    }
    // Prevent amplification when many overlapping ranges exceed the full file.
    if ranges
        .iter()
        .fold(0_u64, |total, (a, b)| total.saturating_add(b - a + 1))
        > size
    {
        ranges.clear();
    }
    Ok(ranges)
}
fn http_time(value: &str) -> Option<SystemTime> {
    httpdate::parse_http_date(value).ok()
}
fn seconds(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH)
        .unwrap_or(Duration::ZERO)
        .as_secs()
}
pub async fn serve(
    file: File,
    path: &str,
    headers: HeaderMap,
    head: bool,
) -> Result<Response, ApiError> {
    let info = file.metadata()?;
    if !info.is_file() {
        return Err(ApiError::new(
            StatusCode::FORBIDDEN,
            "Only regular files can be served",
        ));
    }
    let size = info.len();
    let modified = info.modified().unwrap_or(UNIX_EPOCH);
    let name = path.rsplit('/').next().unwrap_or("download");
    let mut file = tokio::fs::File::from_std(file);
    let mut mime = match mime_guess::from_path(name).first() {
        Some(mime) => mime.to_string(),
        None => {
            let mut prefix = [0u8; 512];
            let n = file.read(&mut prefix).await?;
            file.seek(io::SeekFrom::Start(0)).await?;
            if prefix[..n].starts_with(b"%PDF-") {
                "application/pdf".into()
            } else if prefix[..n].starts_with(b"\x89PNG\r\n\x1a\n") {
                "image/png".into()
            } else if std::str::from_utf8(&prefix[..n]).is_ok()
                && !prefix[..n].iter().any(|b| *b < 9 || (13 < *b && *b < 32))
            {
                "text/plain; charset=utf-8".into()
            } else {
                "application/octet-stream".into()
            }
        }
    };
    if mime.starts_with("text/") && !mime.contains("charset=") {
        mime.push_str("; charset=utf-8");
    }
    // Preview policy is determined by the extension, as in the original app.
    let extension_mime = mime_guess::from_path(name).first_or_octet_stream();
    let preview = extension_mime == "application/pdf"
        || extension_mime == "text/plain"
        || extension_mime.type_() == "image";
    let mut result = response(StatusCode::OK, Body::empty());
    let h = result.headers_mut();
    h.insert("content-type", mime.parse().unwrap());
    h.insert(
        "content-disposition",
        disposition(name, preview)
            .parse()
            .map_err(|_| ApiError::internal("Invalid download name"))?,
    );
    h.insert("x-content-type-options", "nosniff".parse().unwrap());
    h.insert(
        "last-modified",
        httpdate::fmt_http_date(modified).parse().unwrap(),
    );
    h.insert("accept-ranges", "bytes".parse().unwrap());
    if preview {
        h.insert(
            "content-security-policy",
            "sandbox; default-src 'none'; style-src 'unsafe-inline'"
                .parse()
                .unwrap(),
        );
    }
    if !header(&headers, "if-match").is_empty() && header(&headers, "if-match") != "*" {
        *result.status_mut() = StatusCode::PRECONDITION_FAILED;
        return Ok(result);
    }
    if let Some(time) = http_time(header(&headers, "if-unmodified-since"))
        && !headers.contains_key("if-match")
        && seconds(modified) > seconds(time)
    {
        *result.status_mut() = StatusCode::PRECONDITION_FAILED;
        return Ok(result);
    }
    if header(&headers, "if-none-match") == "*"
        || (!headers.contains_key("if-none-match")
            && http_time(header(&headers, "if-modified-since"))
                .is_some_and(|t| seconds(modified) <= seconds(t)))
    {
        result.headers_mut().remove("content-type");
        result.headers_mut().remove("content-disposition");
        *result.status_mut() = StatusCode::NOT_MODIFIED;
        return Ok(result);
    }
    let range = if headers.contains_key("if-range")
        && !http_time(header(&headers, "if-range")).is_some_and(|t| seconds(t) == seconds(modified))
    {
        ""
    } else {
        header(&headers, "range")
    };
    let ranges = if range.is_empty() || head {
        Vec::new()
    } else {
        match parse_ranges(range, size) {
            Ok(ranges) => ranges,
            Err(mut e) => {
                e.headers
                    .insert("content-range", format!("bytes */{size}").parse().unwrap());
                return Err(e);
            }
        }
    };
    if ranges.len() > 1 {
        let boundary = new_id()?;
        let mut length = 0;
        let parts: Vec<_> = ranges.into_iter().map(|(start,end)| {
            let prefix = format!("--{boundary}\r\nContent-Range: bytes {start}-{end}/{size}\r\nContent-Type: {mime}\r\n\r\n");
            length += prefix.len() as u64 + end-start+1 + 2;
            (start,end,prefix)
        }).collect();
        let closing = format!("--{boundary}--\r\n");
        length += closing.len() as u64;
        result.headers_mut().insert(
            "content-type",
            format!("multipart/byteranges; boundary={boundary}")
                .parse()
                .unwrap(),
        );
        result.headers_mut().insert("content-length", length.into());
        *result.status_mut() = StatusCode::PARTIAL_CONTENT;
        let (sender, receiver) = tokio::sync::mpsc::channel::<Result<Bytes, io::Error>>(2);
        tokio::spawn(async move {
            let transfer = async {
                for (start, end, prefix) in parts {
                    if sender.send(Ok(prefix.into())).await.is_err() {
                        return Ok::<_, io::Error>(());
                    }
                    file.seek(io::SeekFrom::Start(start)).await?;
                    let mut remaining = end - start + 1;
                    while remaining > 0 {
                        let mut buf = vec![0; remaining.min(64 * 1024) as usize];
                        let n = file.read(&mut buf).await?;
                        if n == 0 {
                            return Err(io::Error::new(
                                io::ErrorKind::UnexpectedEof,
                                "file changed during download",
                            ));
                        }
                        buf.truncate(n);
                        remaining -= n as u64;
                        if sender.send(Ok(buf.into())).await.is_err() {
                            return Ok(());
                        }
                    }
                    if sender.send(Ok(Bytes::from_static(b"\r\n"))).await.is_err() {
                        return Ok(());
                    }
                }
                let _ = sender.send(Ok(closing.into())).await;
                Ok(())
            }
            .await;
            if let Err(e) = transfer {
                let _ = sender.send(Err(e)).await;
            }
        });
        *result.body_mut() = Body::from_stream(ReceiverStream::new(receiver));
    } else {
        let (start, length) = if let Some((start, end)) = ranges.first().copied() {
            *result.status_mut() = StatusCode::PARTIAL_CONTENT;
            result.headers_mut().insert(
                "content-range",
                format!("bytes {start}-{end}/{size}").parse().unwrap(),
            );
            (start, end - start + 1)
        } else {
            (0, size)
        };
        result.headers_mut().insert("content-length", length.into());
        if !head {
            file.seek(io::SeekFrom::Start(start)).await?;
            *result.body_mut() = Body::from_stream(ReaderStream::new(file.take(length)));
        }
    }
    Ok(result)
}
