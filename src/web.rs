use crate::{
    filesystem::user_path,
    listing,
    state::{App, Expiry},
    upload,
};
use axum::{
    body::Body,
    extract::{Request, State},
    http::{HeaderMap, Method, StatusCode},
    response::{IntoResponse, Response},
};
use chrono::Utc;
use percent_encoding::percent_decode_str;
use rustix::fs::OFlags;
use std::{io, sync::Arc, time::Instant};

// Account for streamed downloads until the body finishes or the client leaves.
struct MeasuredBody {
    inner: Body,
    app: Arc<App>,
    method: Method,
    started: Instant,
    recorded: bool,
}
impl MeasuredBody {
    fn finish(&mut self) {
        if !self.recorded {
            self.app
                .metrics
                .observe_duration(self.method.as_str(), self.started.elapsed().as_secs_f64());
            self.recorded = true;
        }
    }
}
impl axum::body::HttpBody for MeasuredBody {
    type Data = axum::body::Bytes;
    type Error = axum::Error;
    fn poll_frame(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        let result = std::pin::Pin::new(&mut self.inner).poll_frame(cx);
        if matches!(result, std::task::Poll::Ready(None | Some(Err(_)))) {
            self.finish();
        }
        result
    }
    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }
    fn size_hint(&self) -> http_body::SizeHint {
        self.inner.size_hint()
    }
}
impl Drop for MeasuredBody {
    fn drop(&mut self) {
        self.finish();
    }
}

#[derive(Debug)]
pub struct ApiError {
    pub status: StatusCode,
    detail: String,
    kind: String,
    pub headers: Box<HeaderMap>,
}
impl ApiError {
    pub fn new(status: StatusCode, detail: impl Into<String>) -> Self {
        Self {
            status,
            detail: detail.into(),
            kind: String::new(),
            headers: Box::default(),
        }
    }
    pub fn internal(detail: impl Into<String>) -> Self {
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, detail)
    }
    pub fn bad(detail: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, detail)
    }
    pub fn not_found() -> Self {
        Self::new(StatusCode::NOT_FOUND, "Not found")
    }
    pub fn method(allow: &str) -> Self {
        let mut e = Self::new(StatusCode::METHOD_NOT_ALLOWED, "Method not allowed");
        e.headers.insert("allow", allow.parse().unwrap());
        e
    }
}
impl From<io::Error> for ApiError {
    fn from(e: io::Error) -> Self {
        let status = match e.kind() {
            io::ErrorKind::NotFound => StatusCode::NOT_FOUND,
            io::ErrorKind::AlreadyExists => StatusCode::CONFLICT,
            io::ErrorKind::PermissionDenied => StatusCode::FORBIDDEN,
            _ if matches!(e.raw_os_error(), Some(20 | 40)) => StatusCode::FORBIDDEN,
            _ => StatusCode::INTERNAL_SERVER_ERROR,
        };
        if status == StatusCode::INTERNAL_SERVER_ERROR {
            eprintln!("Filesystem operation: {e}");
        }
        Self::new(
            status,
            if status == StatusCode::INTERNAL_SERVER_ERROR {
                "Unable to access storage".into()
            } else {
                e.to_string()
            },
        )
    }
}
impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let mut json = serde_json::json!({"title": self.status.canonical_reason().unwrap_or("Error"), "status": self.status.as_u16(), "detail": self.detail});
        if !self.kind.is_empty() {
            json["type"] = self.kind.into();
        }
        let mut result = response(self.status, json.to_string());
        *result.headers_mut() = *self.headers;
        result
            .headers_mut()
            .insert("content-type", "application/problem+json".parse().unwrap());
        result
    }
}
pub fn problem(status: StatusCode, kind: &str, detail: &str) -> ApiError {
    let mut e = ApiError::new(status, detail);
    e.kind = kind.into();
    e
}
pub fn response(status: StatusCode, body: impl Into<Body>) -> Response {
    let mut r = Response::new(body.into());
    *r.status_mut() = status;
    r
}
pub fn header<'a>(headers: &'a HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|h| h.to_str().ok())
        .unwrap_or("")
}
fn sf_item(headers: &HeaderMap, name: &str) -> Result<sfv::Item, ApiError> {
    let all = headers.get_all(name);
    if all.iter().count() != 1 {
        return Err(ApiError::bad(format!("invalid or missing {name} header")));
    }
    sfv::Parser::new(header(headers, name))
        .parse()
        .map_err(|_| ApiError::bad(format!("invalid {name} header")))
}
pub fn sf_bool(headers: &HeaderMap, name: &str) -> Result<bool, ApiError> {
    match sf_item(headers, name)?.bare_item {
        sfv::BareItem::Boolean(v) => Ok(v),
        _ => Err(ApiError::bad(format!("invalid {name} header"))),
    }
}
pub fn sf_integer(headers: &HeaderMap, name: &str) -> Result<u64, ApiError> {
    match sf_item(headers, name)?.bare_item {
        sfv::BareItem::Integer(v) if i64::from(v) >= 0 => Ok(i64::from(v) as u64),
        _ => Err(ApiError::bad(format!("invalid {name} header"))),
    }
}
pub fn target(app: &App, path: &str) -> Result<String, ApiError> {
    if path.ends_with('/') {
        return Err(ApiError::bad("cannot upload to a directory path"));
    }
    let rel =
        user_path(path).map_err(|_| ApiError::new(StatusCode::FORBIDDEN, "Invalid file path"))?;
    if rel == "." {
        return Err(ApiError::bad("Invalid file path"));
    }
    app.files.validate_existing(&rel)?;
    Ok(rel)
}
fn check_origin(method: &Method, headers: &HeaderMap) -> Result<(), ApiError> {
    if matches!(*method, Method::GET | Method::HEAD | Method::OPTIONS) {
        return Ok(());
    }
    let site = header(headers, "sec-fetch-site");
    let origin = header(headers, "origin");
    let allowed = match site {
        "same-origin" | "none" => true,
        "" => {
            origin.is_empty()
                || url::Url::parse(origin).is_ok_and(|u| {
                    let authority = &u[url::Position::BeforeHost..url::Position::AfterPort];
                    matches!(u.scheme(), "http" | "https")
                        && authority.eq_ignore_ascii_case(header(headers, "host"))
                        && u.username().is_empty()
                        && u.password().is_none()
                })
        }
        _ => false,
    };
    if allowed {
        Ok(())
    } else {
        Err(ApiError::new(
            StatusCode::FORBIDDEN,
            "Cross-origin file changes are forbidden",
        ))
    }
}
fn query_value(query: &str, key: &str) -> Option<String> {
    url::form_urlencoded::parse(query.as_bytes())
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.into_owned())
}
fn temporary(query: &str) -> Result<bool, ApiError> {
    match query_value(query, "temporary").as_deref() {
        None | Some("" | "false") => Ok(false),
        Some("true") => Ok(true),
        _ => Err(ApiError::bad("temporary must be true or false")),
    }
}
pub async fn handler(State(app): State<Arc<App>>, request: Request) -> Response {
    let started = Instant::now();
    let method = request.method().clone();
    let metrics = request.uri().path() == "/metrics";
    let is_upload = matches!(method, Method::PUT | Method::POST)
        && query_value(request.uri().query().unwrap_or(""), "folder").as_deref() != Some("true")
        || (method == Method::PATCH && request.uri().path().starts_with(upload::PREFIX));
    let result = dispatch(app.clone(), request).await;
    let response = match result {
        Ok(r) => r,
        Err(e) => e.into_response(),
    };
    if !metrics {
        app.metrics.record_request(response.status().as_u16() < 400);
    }
    if is_upload && app.config.upload {
        app.metrics.uploads.with_label_values(&["total"]).inc();
        app.metrics
            .uploads
            .with_label_values(&[if response.status().as_u16() < 400 {
                "success"
            } else {
                "error"
            }])
            .inc();
    }
    // HEAD must never send a body, including error responses.
    let (parts, body) = response.into_parts();
    let body = if method == Method::HEAD {
        Body::empty()
    } else {
        body
    };
    if metrics {
        return Response::from_parts(parts, body);
    }
    Response::from_parts(
        parts,
        Body::new(MeasuredBody {
            inner: body,
            app,
            method,
            started,
            recorded: false,
        }),
    )
}
async fn dispatch(app: Arc<App>, request: Request) -> Result<Response, ApiError> {
    let (parts, body) = request.into_parts();
    let method = parts.method;
    let headers = parts.headers;
    let uri = parts.uri;
    if uri.path() == "/metrics" {
        if !app.config.metrics {
            return Err(ApiError::new(StatusCode::FORBIDDEN, "Metrics are disabled"));
        }
        let mut result = response(
            StatusCode::OK,
            app.metrics
                .encode()
                .map_err(|e| ApiError::internal(e.to_string()))?,
        );
        result.headers_mut().insert(
            "content-type",
            "text/plain; version=0.0.4; charset=utf-8".parse().unwrap(),
        );
        return Ok(result);
    }
    check_origin(&method, &headers)?;
    let path = percent_decode_str(uri.path())
        .decode_utf8()
        .map_err(|_| ApiError::bad("Invalid URL encoding"))?
        .into_owned();
    if path.starts_with(upload::PREFIX) {
        if !app.config.upload {
            return Err(ApiError::new(
                StatusCode::FORBIDDEN,
                "File uploads are disabled",
            ));
        }
        return upload::resource(app, method, &path, headers, body).await;
    }
    if path.split('/').any(|p| p == crate::filesystem::PRIVATE) {
        return Err(ApiError::not_found());
    }
    let query = uri.query().unwrap_or("");
    if matches!(
        method,
        Method::PUT | Method::POST | Method::PATCH | Method::DELETE
    ) && !app.config.upload
    {
        return Err(ApiError::new(
            StatusCode::FORBIDDEN,
            "File changes are disabled",
        ));
    }
    match method {
        Method::OPTIONS => return Ok(upload::options(false)),
        Method::PUT => {
            let temp = temporary(query)?;
            if headers.contains_key("upload-complete") {
                return upload::create(app, &path, headers, temp, body).await;
            }
            return upload::put(app, &path, temp, body).await;
        }
        Method::POST => {
            if headers.contains_key("upload-complete") {
                let temp = temporary(query)?;
                return upload::create(app, &path, headers, temp, body).await;
            }
            if query_value(query, "folder").as_deref() == Some("true") {
                let path = path.clone();
                let location = uri.path().to_owned();
                return tokio::task::spawn_blocking(move || {
                    let rel = user_path(&path).map_err(|_| ApiError::bad("Invalid folder name"))?;
                    if rel == "." {
                        return Err(ApiError::bad("Invalid folder name"));
                    }
                    let _actions = app.actions.lock().unwrap();
                    app.files.validate_existing(&rel)?;
                    app.files.mkdir(&rel)?;
                    app.invalidate_sizes(&rel);
                    let mut result = response(StatusCode::CREATED, Body::empty());
                    result
                        .headers_mut()
                        .insert("location", location.parse().unwrap());
                    Ok(result)
                })
                .await
                .map_err(|e| ApiError::internal(e.to_string()))?;
            }
            return Err(ApiError::method(
                "GET, HEAD, PUT, POST, PATCH, DELETE, OPTIONS",
            ));
        }
        Method::DELETE | Method::PATCH => {
            let remove = method == Method::DELETE;
            let temp = if remove {
                false
            } else {
                match query_value(query, "temporary").as_deref() {
                    Some("true") => true,
                    Some("false") => false,
                    _ => return Err(ApiError::bad("Specify temporary=true or temporary=false")),
                }
            };
            let rel = target(&app, &path)?;
            return tokio::task::spawn_blocking(move || {
                let mut entries = app.actions.lock().unwrap();
                let file = app.files.open_file(&rel, OFlags::RDONLY, 0)?;
                let info = file.metadata()?;
                if !info.is_file() {
                    return Err(ApiError::bad(
                        "Only regular files can be deleted or made temporary",
                    ));
                }
                if remove {
                    app.files.remove(&rel)?;
                    if entries.remove(&rel).is_some() {
                        app.save_expirations(&entries)?;
                    }
                    app.invalidate_sizes(&rel);
                    return Ok(response(StatusCode::NO_CONTENT, Body::empty()));
                }
                let old = entries.get(&rel).cloned();
                let expires = if temp {
                    Utc::now() + chrono::Duration::minutes(5)
                } else {
                    chrono::DateTime::UNIX_EPOCH
                };
                if temp {
                    entries.insert(
                        rel.clone(),
                        Expiry {
                            expires_at: expires,
                            modified: info.modified()?.into(),
                            size: info.len(),
                        },
                    );
                } else {
                    entries.remove(&rel);
                }
                if let Err(e) = app.save_expirations(&entries) {
                    if let Some(old) = old {
                        entries.insert(rel, old);
                    } else {
                        entries.remove(&rel);
                    }
                    return Err(e.into());
                }
                let mut result = response(
                    StatusCode::OK,
                    serde_json::json!({"expiresAt": expires.timestamp()}).to_string(),
                );
                result
                    .headers_mut()
                    .insert("content-type", "application/json".parse().unwrap());
                Ok(result)
            })
            .await
            .map_err(|e| ApiError::internal(e.to_string()))?;
        }
        Method::GET | Method::HEAD => (),
        _ => {
            return Err(ApiError::method(
                "GET, HEAD, PUT, POST, PATCH, DELETE, OPTIONS",
            ));
        }
    }
    let rel =
        user_path(&path).map_err(|_| ApiError::new(StatusCode::FORBIDDEN, "Invalid file path"))?;
    let a = app.clone();
    let r = rel.clone();
    let file = tokio::task::spawn_blocking(move || a.files.open_file(&r, OFlags::RDONLY, 0))
        .await
        .map_err(|e| ApiError::internal(e.to_string()))??;
    if file.metadata()?.is_dir() {
        if !path.ends_with('/') {
            let mut result = response(StatusCode::FOUND, Body::empty());
            result.headers_mut().insert(
                "location",
                format!(
                    "{}/{}",
                    uri.path(),
                    uri.query().map_or(String::new(), |q| format!("?{q}"))
                )
                .parse()
                .map_err(|_| ApiError::bad("Invalid URL"))?,
            );
            return Ok(result);
        }
        app.metrics
            .operations
            .with_label_values(&["directory_list"])
            .inc();
        listing::directory(app, uri, headers, path, rel, method == Method::HEAD).await
    } else {
        app.metrics
            .operations
            .with_label_values(&["file_serve"])
            .inc();
        crate::download::serve(file, &rel, headers, method == Method::HEAD).await
    }
}
