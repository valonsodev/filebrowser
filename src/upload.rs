use crate::{
    filesystem::{new_id, user_path},
    state::App,
    web::{ApiError, header, problem, response, sf_bool, sf_integer, target},
};
use axum::{
    body::Body,
    http::{HeaderMap, Method, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use futures_util::StreamExt;
use rustix::fs::OFlags;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    io::{self, Read},
    sync::Arc,
};
use tokio::io::{AsyncSeekExt, AsyncWriteExt};

pub const PREFIX: &str = "/.uploads/";
pub const MEDIA: &str = "application/partial-upload";
pub const AGE: i64 = 24 * 60 * 60;
const LENGTH_PROBLEM: &str =
    "https://iana.org/assignments/http-problem-types#inconsistent-upload-length";
const OFFSET_PROBLEM: &str =
    "https://iana.org/assignments/http-problem-types#mismatching-upload-offset";
const COMPLETE_PROBLEM: &str = "https://iana.org/assignments/http-problem-types#completed-upload";
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Upload {
    pub version: u8,
    pub id: String,
    pub target: String,
    #[serde(default)]
    pub sha256: String,
    #[serde(default)]
    pub client_id: String,
    pub offset: u64,
    pub length: u64,
    pub length_known: bool,
    pub complete: bool,
    pub temporary: bool,
    #[serde(default)]
    pub password_hash: Option<String>,
    #[serde(default)]
    pub expected_password_hash: Option<String>,
    #[serde(default)]
    pub expires_at: i64,
    #[serde(default)]
    pub deleted: bool,
    #[serde(default)]
    pub finalizing: bool,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}
// Immutable identity permits discovery to skip unrelated transfers without
// waiting for their per-resource lock.
pub struct UploadEntry {
    target: String,
    sha256: String,
    client_id: String,
    temporary: bool,
    pub state: tokio::sync::Mutex<Upload>,
}
impl UploadEntry {
    fn new(upload: Upload) -> Self {
        Self {
            target: upload.target.clone(),
            sha256: upload.sha256.clone(),
            client_id: upload.client_id.clone(),
            temporary: upload.temporary,
            state: tokio::sync::Mutex::new(upload),
        }
    }
}

pub fn valid_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}
impl Upload {
    fn location(&self) -> String {
        format!("{PREFIX}{}", self.id)
    }
    pub fn persist(&self, app: &App) -> io::Result<()> {
        app.store()
            .atomic_write(&format!("{}.json", self.id), &serde_json::to_vec(self)?)
    }
    pub fn discard(&mut self, app: &App) -> io::Result<()> {
        self.deleted = true;
        let marker = self.persist(app);
        app.uploads.lock().unwrap().remove(&self.id);
        app.store().remove(&self.id)?;
        match app.store().remove(&format!("{}.json", self.id)) {
            Ok(()) => app.store().sync(),
            Err(e) => {
                marker?;
                Err(e)
            }
        }
    }
    fn headers(&self, result: &mut Response) {
        let headers = result.headers_mut();
        headers.insert("upload-offset", self.offset.into());
        headers.insert(
            "upload-complete",
            if self.complete { "?1" } else { "?0" }.parse().unwrap(),
        );
        if self.length_known {
            headers.insert("upload-length", self.length.into());
        }
        if !self.client_id.is_empty() {
            headers.insert("upload-id", self.client_id.parse().unwrap());
        }
        if !self.sha256.is_empty() {
            headers.insert("upload-sha256", self.sha256.parse().unwrap());
        }
        if self.complete
            && let Some(hash) = &self.password_hash
        {
            headers.insert(
                "file-location",
                crate::password::location(&self.target, hash)
                    .parse()
                    .unwrap(),
            );
        }
        if self.complete && self.expires_at > 0 {
            headers.insert("file-expires-at", self.expires_at.into());
        }
    }
    pub fn finalize(&mut self, app: &App) -> Result<(), ApiError> {
        if self.length_known && self.length != self.offset {
            return Err(problem(
                StatusCode::BAD_REQUEST,
                LENGTH_PROBLEM,
                "upload length does not match declared length",
            ));
        }
        if !self.sha256.is_empty() {
            let digest = hash_reader(app.store().open_file(&self.id, OFlags::RDONLY, 0)?)?;
            if digest != self.sha256 {
                self.discard(app)?;
                return Err(ApiError::new(
                    StatusCode::UNPROCESSABLE_ENTITY,
                    "uploaded file does not match Upload-SHA256",
                ));
            }
            self.finalizing = true;
            self.persist(app)?;
        }
        self.expires_at = app.install(
            &self.id,
            &self.target,
            self.temporary,
            self.password_hash.as_deref(),
            self.expected_password_hash.as_deref(),
        )?;
        self.complete = true;
        self.finalizing = false;
        self.length = self.offset;
        self.length_known = true;
        self.updated_at = Utc::now();
        self.persist(app)?;
        app.metrics
            .resumable
            .with_label_values(&["completed"])
            .inc();
        Ok(())
    }
}
fn hash_reader(mut file: std::fs::File) -> io::Result<String> {
    let mut hash = Sha256::new();
    let mut buf = vec![0; 1024 * 1024];
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hash.update(&buf[..n]);
    }
    Ok(hex::encode(hash.finalize()))
}
impl App {
    pub fn restore_uploads(&self) -> io::Result<()> {
        let entries = self.store().names(".")?;
        let mut loaded = std::collections::HashMap::new();
        for name in &entries {
            let Some(id) = name.strip_suffix(".json").filter(|id| valid_hex(id, 32)) else {
                continue;
            };
            let result = (|| -> io::Result<Option<Upload>> {
                let mut u: Upload = serde_json::from_slice(&self.store().read(name, 64 * 1024)?)?;
                if u.version != 1
                    || u.id != id
                    || u.created_at.timestamp() <= 0
                    || u.updated_at.timestamp() <= 0
                    || (u.length_known && u.offset > u.length)
                    || (u.complete && (!u.length_known || u.offset != u.length))
                    || (!u.sha256.is_empty() && (!valid_hex(&u.sha256, 64) || !u.length_known))
                    || (!u.client_id.is_empty() && !valid_hex(&u.client_id, 32))
                    || user_path(&u.target).ok().as_deref() != Some(u.target.as_str())
                    || u.target == "."
                {
                    return Err(io::Error::other("invalid upload state"));
                }
                self.files.validate_existing(&u.target)?;
                if u.deleted || (Utc::now() - u.updated_at).num_seconds() >= AGE {
                    return Ok(None);
                }
                if !u.complete {
                    match self.store().open_file(id, OFlags::RDONLY, 0) {
                        Ok(file) => {
                            let info = file.metadata()?;
                            if !info.is_file() || (u.length_known && info.len() > u.length) {
                                return Err(io::Error::other("invalid partial file"));
                            }
                            u.offset = info.len();
                        }
                        Err(e)
                            if e.kind() == io::ErrorKind::NotFound
                                && u.finalizing
                                && !u.sha256.is_empty() =>
                        {
                            let file = self.files.open_file(&u.target, OFlags::RDONLY, 0)?;
                            if !file.metadata()?.is_file()
                                || file.metadata()?.len() != u.length
                                || hash_reader(file)? != u.sha256
                            {
                                return Err(io::Error::other(
                                    "installed file does not match upload",
                                ));
                            }
                            u.offset = u.length;
                            u.complete = true;
                            u.finalizing = false;
                            u.expires_at = self
                                .actions
                                .lock()
                                .unwrap()
                                .get(&u.target)
                                .map_or(0, |e| e.expires_at.timestamp());
                        }
                        Err(e) => return Err(e),
                    }
                }
                u.persist(self)?;
                Ok(Some(u))
            })();
            match result {
                Ok(Some(u)) => {
                    loaded.insert(id.to_owned(), Arc::new(UploadEntry::new(u)));
                }
                other => {
                    if let Err(e) = other {
                        eprintln!("Ignoring upload metadata {name}: {e}");
                    }
                    self.store().remove(name)?;
                    self.store().remove(id)?;
                }
            }
        }
        for name in entries {
            if (valid_hex(&name, 32) && !loaded.contains_key(&name))
                || name.starts_with(".state-")
                || name.starts_with("put-")
            {
                self.store().remove(&name)?;
            }
        }
        *self.uploads.lock().unwrap() = loaded;
        self.store().sync()
    }
    pub fn expire_uploads(&self) -> io::Result<()> {
        let uploads: Vec<_> = self.uploads.lock().unwrap().values().cloned().collect();
        for upload in uploads {
            if let Ok(mut upload) = upload.state.try_lock()
                && (Utc::now() - upload.updated_at).num_seconds() >= AGE
            {
                upload.discard(self)?;
            }
        }
        Ok(())
    }
}
/// Stream with bounded memory. Client disconnection keeps the durably received prefix.
async fn write_body(
    app: Arc<App>,
    id: String,
    offset: u64,
    body: Body,
) -> Result<(u64, bool), ApiError> {
    let a = app.clone();
    let file = tokio::task::spawn_blocking(move || {
        a.store()
            .open_file(&id, OFlags::WRONLY | OFlags::CREATE, 0o644)
    })
    .await
    .map_err(|e| ApiError::internal(e.to_string()))??;
    let mut file = tokio::fs::File::from_std(file);
    file.seek(std::io::SeekFrom::Start(offset)).await?;
    let mut stream = body.into_data_stream();
    let mut count = 0;
    let mut interrupted = false;
    while let Some(chunk) = stream.next().await {
        match chunk {
            Ok(bytes) => {
                file.write_all(&bytes).await?;
                count += bytes.len() as u64;
            }
            Err(_) => {
                interrupted = true;
                break;
            }
        }
    }
    file.flush().await?;
    file.sync_all().await?;
    Ok((count, interrupted))
}
async fn persist(app: Arc<App>, u: &Upload) -> Result<(), ApiError> {
    let u = u.clone();
    tokio::task::spawn_blocking(move || u.persist(&app))
        .await
        .map_err(|e| ApiError::internal(e.to_string()))??;
    Ok(())
}
async fn finalize(app: Arc<App>, u: &mut Upload) -> Result<(), ApiError> {
    let mut owned = u.clone();
    let (new, result) = tokio::task::spawn_blocking(move || {
        let result = owned.finalize(&app);
        (owned, result)
    })
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;
    *u = new;
    result
}
fn completion(u: &Upload) -> Response {
    let mut result = response(
        StatusCode::CREATED,
        format!("File uploaded successfully to /{}\n", u.target),
    );
    u.headers(&mut result);
    result
}
pub async fn put(
    app: Arc<App>,
    path: &str,
    temporary: bool,
    headers: HeaderMap,
    body: Body,
) -> Result<Response, ApiError> {
    let target = target(&app, path)?;
    let expected = crate::password::authorize(&app, &target, &headers).await?;
    let password_hash = crate::password::for_upload(&app, &headers, &expected).await?;
    let id = format!("put-{}", new_id()?);
    let result = async {
        let (_, interrupted) = write_body(app.clone(), id.clone(), 0, body).await?;
        if interrupted {
            return Err(ApiError::internal("Error receiving file"));
        }
        let a = app.clone();
        let i = id.clone();
        let installed_target = target.clone();
        let expires = tokio::task::spawn_blocking(move || {
            a.install(
                &i,
                &target,
                temporary,
                password_hash.as_deref(),
                expected.as_deref(),
            )
        })
        .await
        .map_err(|e| ApiError::internal(e.to_string()))??;
        let mut result = response(
            StatusCode::CREATED,
            format!("File uploaded successfully to {path}\n"),
        );
        if let Some(hash) = app.protections.lock().unwrap().get(&installed_target) {
            result.headers_mut().insert(
                "file-location",
                crate::password::location(&installed_target, hash)
                    .parse()
                    .unwrap(),
            );
        }
        if expires > 0 {
            result
                .headers_mut()
                .insert("file-expires-at", expires.into());
        }
        Ok(result)
    }
    .await;
    let _ = app.store().remove(&id);
    result
}
pub async fn create(
    app: Arc<App>,
    path: &str,
    headers: HeaderMap,
    temporary: bool,
    body: Body,
) -> Result<Response, ApiError> {
    let complete = sf_bool(&headers, "upload-complete")?;
    let target = target(&app, path)?;
    let expected = crate::password::authorize(&app, &target, &headers).await?;
    let content_length = header(&headers, "content-length").parse::<u64>().ok();
    let content_length = if content_length.is_none() && !headers.contains_key("transfer-encoding") {
        Some(0)
    } else {
        content_length
    };
    let from_content = if complete { content_length } else { None };
    let length = if headers.contains_key("upload-length") {
        Some(sf_integer(&headers, "upload-length")?)
    } else {
        from_content
    };
    if let (Some(a), Some(b)) = (from_content, length)
        && a != b
    {
        return Err(problem(
            StatusCode::BAD_REQUEST,
            LENGTH_PROBLEM,
            "Upload-Length and Content-Length disagree",
        ));
    }
    let hash = header(&headers, "upload-sha256").to_owned();
    let client_id = header(&headers, "upload-id").to_owned();
    if !client_id.is_empty()
        && (!valid_hex(&client_id, 32)
            || length.is_none()
            || complete
            || content_length != Some(0)
            || headers.get_all("upload-id").iter().count() != 1)
    {
        return Err(ApiError::bad(
            "Upload-ID requires 32 lowercase hex digits, Upload-Length, Upload-Complete: ?0 and an empty body",
        ));
    }
    let _discovery = if !hash.is_empty() || !client_id.is_empty() {
        if !hash.is_empty()
            && (!valid_hex(&hash, 64)
                || length.is_none()
                || complete
                || content_length != Some(0)
                || headers.get_all("upload-sha256").iter().count() != 1)
        {
            return Err(ApiError::bad(
                "Upload-SHA256 requires a lowercase SHA-256 digest, Upload-Length, Upload-Complete: ?0 and an empty body",
            ));
        }
        Some(app.discovery.lock().await)
    } else {
        None
    };
    if !hash.is_empty() || !client_id.is_empty() {
        let entries: Vec<_> = app.uploads.lock().unwrap().values().cloned().collect();
        for entry in entries {
            if entry.target != target
                || entry.sha256 != hash
                || entry.client_id != client_id
                || entry.temporary != temporary
            {
                continue;
            }
            let mut u = entry.state.lock().await;
            if !u.deleted
                && !u.complete
                && u.target == target
                && u.sha256 == hash
                && u.client_id == client_id
                && Some(u.length) == length
                && u.length_known
                && u.temporary == temporary
                && (Utc::now() - u.updated_at).num_seconds() < AGE
            {
                crate::password::verify(&app, &headers, u.password_hash.clone()).await?;
                if u.password_hash.is_none() && headers.contains_key(crate::password::HEADER) {
                    continue;
                }
                u.updated_at = Utc::now();
                persist(app.clone(), &u).await?;
                let mut result = response(StatusCode::OK, Body::empty());
                u.headers(&mut result);
                result
                    .headers_mut()
                    .insert("location", u.location().parse().unwrap());
                result
                    .headers_mut()
                    .insert("cache-control", "no-store".parse().unwrap());
                return Ok(result);
            }
        }
    }
    let password_hash = crate::password::for_upload(&app, &headers, &expected).await?;
    let id = new_id()?;
    let mut u = Upload {
        version: 1,
        id: id.clone(),
        target,
        sha256: hash,
        client_id,
        offset: 0,
        length: length.unwrap_or(0),
        length_known: length.is_some(),
        complete: false,
        temporary,
        password_hash,
        expected_password_hash: expected,
        expires_at: 0,
        deleted: false,
        finalizing: false,
        created_at: Utc::now(),
        updated_at: Utc::now(),
    };
    let (count, interrupted) = write_body(app.clone(), id.clone(), 0, body).await?;
    u.offset = count;
    u.updated_at = Utc::now();
    if u.length_known && u.offset > u.length {
        u.discard(&app)?;
        return Err(problem(
            StatusCode::BAD_REQUEST,
            LENGTH_PROBLEM,
            "received more data than declared length",
        ));
    }
    if let Err(e) = persist(app.clone(), &u).await {
        let _ = u.discard(&app);
        return Err(e);
    }
    let entry = Arc::new(UploadEntry::new(u));
    let mut u = entry.state.lock().await;
    app.uploads.lock().unwrap().insert(id, entry.clone());
    app.metrics.resumable.with_label_values(&["created"]).inc();
    if complete && !interrupted {
        finalize(app.clone(), &mut u).await?;
        return Ok(completion(&u));
    }
    let mut result = response(StatusCode::CREATED, Body::empty());
    u.headers(&mut result);
    result
        .headers_mut()
        .insert("location", u.location().parse().unwrap());
    Ok(result)
}
pub async fn resource(
    app: Arc<App>,
    method: Method,
    path: &str,
    headers: HeaderMap,
    body: Body,
) -> Result<Response, ApiError> {
    let id = path
        .strip_prefix(PREFIX)
        .filter(|id| valid_hex(id, 32))
        .ok_or_else(ApiError::not_found)?;
    if method == Method::OPTIONS {
        return Ok(options(true));
    }
    if !matches!(method, Method::HEAD | Method::PATCH | Method::DELETE) {
        return Err(ApiError::method("HEAD, PATCH, DELETE, OPTIONS"));
    }
    let entry = app
        .uploads
        .lock()
        .unwrap()
        .get(id)
        .cloned()
        .ok_or_else(ApiError::not_found)?;
    let mut u = if method == Method::PATCH {
        entry.state.try_lock().map_err(|_| {
            ApiError::new(
                StatusCode::CONFLICT,
                "another transfer is in progress for this upload",
            )
        })?
    } else {
        entry.state.lock().await
    };
    crate::password::verify(&app, &headers, u.password_hash.clone()).await?;
    if u.deleted {
        return Err(ApiError::not_found());
    }
    if method == Method::HEAD {
        let mut result = response(StatusCode::NO_CONTENT, Body::empty());
        u.headers(&mut result);
        result
            .headers_mut()
            .insert("cache-control", "no-store".parse().unwrap());
        return Ok(result);
    }
    if method == Method::DELETE {
        u.discard(&app)?;
        app.metrics.resumable.with_label_values(&["canceled"]).inc();
        return Ok(response(StatusCode::NO_CONTENT, Body::empty()));
    }
    app.metrics.resumable.with_label_values(&["append"]).inc();

    append(app.clone(), &mut u, headers, body).await
}
async fn append(
    app: Arc<App>,
    u: &mut Upload,
    headers: HeaderMap,
    body: Body,
) -> Result<Response, ApiError> {
    if !header(&headers, "content-type")
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .eq_ignore_ascii_case(MEDIA)
    {
        return Err(ApiError::new(
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            format!("Content-Type must be {MEDIA}"),
        ));
    }
    let complete = sf_bool(&headers, "upload-complete")?;
    let offset = sf_integer(&headers, "upload-offset")?;
    if u.complete {
        let mut e = problem(
            StatusCode::CONFLICT,
            COMPLETE_PROBLEM,
            "upload is already complete",
        );
        let mut r = response(e.status, Body::empty());
        u.headers(&mut r);
        r.headers_mut().insert(
            "location",
            format!("/{}", crate::listing::encode_path(&u.target))
                .parse()
                .unwrap(),
        );
        e.headers = Box::new(r.headers().clone());
        return Err(e);
    }
    if offset != u.offset {
        let mut e = problem(
            StatusCode::CONFLICT,
            OFFSET_PROBLEM,
            "Upload-Offset does not match the current offset",
        );
        let mut r = response(e.status, Body::empty());
        u.headers(&mut r);
        e.headers = Box::new(r.headers().clone());
        return Err(e);
    }
    let (count, interrupted) = write_body(app.clone(), u.id.clone(), u.offset, body).await?;
    u.offset += count;
    u.updated_at = Utc::now();
    if u.length_known && u.offset > u.length {
        u.discard(&app)?;
        return Err(problem(
            StatusCode::BAD_REQUEST,
            LENGTH_PROBLEM,
            "received more data than declared length",
        ));
    }
    persist(app.clone(), u).await?;
    if complete && !interrupted {
        finalize(app, u).await?;
        return Ok(completion(u));
    }
    let mut result = response(StatusCode::NO_CONTENT, Body::empty());
    u.headers(&mut result);
    Ok(result)
}
pub fn options(resource: bool) -> Response {
    let mut result = response(StatusCode::NO_CONTENT, Body::empty());
    result
        .headers_mut()
        .insert("accept-patch", MEDIA.parse().unwrap());
    result.headers_mut().insert(
        "allow",
        if resource {
            "HEAD, PATCH, DELETE, OPTIONS"
        } else {
            "GET, HEAD, PUT, POST, PATCH, DELETE, OPTIONS"
        }
        .parse()
        .unwrap(),
    );
    result
}
