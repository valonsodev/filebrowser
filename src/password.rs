//! Header-only credentials; only salted Argon2id verifiers reach durable storage.
use crate::{state::App, web::ApiError};
use argon2::{
    Argon2,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
};
use axum::http::{HeaderMap, StatusCode};
use std::{
    sync::{Arc, Mutex},
    time::Instant,
};
use zeroize::Zeroizing;

pub const HEADER: &str = "file-password";
pub struct Work {
    slots: Arc<tokio::sync::Semaphore>,
    rate: Mutex<(Instant, f64)>,
}
impl Default for Work {
    fn default() -> Self {
        Self {
            slots: Arc::new(tokio::sync::Semaphore::new(2)),
            rate: Mutex::new((Instant::now(), 20.0)),
        }
    }
}
fn denied() -> ApiError {
    let mut error = ApiError::new(
        StatusCode::UNAUTHORIZED,
        "A valid file password is required",
    );
    error.headers.insert(
        "www-authenticate",
        "FilePassword realm=\"file\"".parse().unwrap(),
    );
    error
}
fn busy() -> ApiError {
    let mut error = ApiError::new(
        StatusCode::TOO_MANY_REQUESTS,
        "Too many password attempts; try again shortly",
    );
    error.headers.insert("retry-after", "1".parse().unwrap());
    error
}
pub fn credential(headers: &HeaderMap) -> Result<Option<Zeroizing<String>>, ApiError> {
    if !headers.contains_key(HEADER) {
        return Ok(None);
    }
    if headers.get_all(HEADER).iter().count() != 1 {
        return Err(ApiError::bad("Supply one File-Password header"));
    }
    let value = headers[HEADER]
        .to_str()
        .map_err(|_| ApiError::bad("Invalid File-Password header"))?;
    if value.is_empty() || value.len() > 1024 || !value.bytes().all(|b| (32..=126).contains(&b)) {
        return Err(ApiError::bad(
            "File passwords must contain 1–1024 printable ASCII characters",
        ));
    }
    Ok(Some(Zeroizing::new(value.to_owned())))
}
async fn work(
    app: &App,
    password: Zeroizing<String>,
    hash: Option<String>,
) -> Result<String, ApiError> {
    {
        let mut rate = app.password_work.rate.lock().unwrap();
        let now = Instant::now();
        rate.1 = (rate.1 + now.duration_since(rate.0).as_secs_f64() * 10.0).min(20.0);
        rate.0 = now;
        if rate.1 < 1.0 {
            return Err(busy());
        }
        rate.1 -= 1.0;
    }
    // Short, rate-limited waiting permits the browser's three parallel uploads
    // while retaining a strict bound on memory-heavy Argon2 workers.
    let permit = tokio::time::timeout(
        std::time::Duration::from_secs(2),
        app.password_work.slots.clone().acquire_owned(),
    )
    .await
    .map_err(|_| busy())?
    .map_err(|_| busy())?;
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        if let Some(hash) = hash {
            let parsed = PasswordHash::new(&hash)
                .map_err(|_| ApiError::internal("Invalid password metadata"))?;
            Argon2::default()
                .verify_password(password.as_bytes(), &parsed)
                .map_err(|_| denied())?;
            Ok(hash)
        } else {
            let mut bytes = [0; 16];
            getrandom::fill(&mut bytes)
                .map_err(|_| ApiError::internal("Unable to generate password salt"))?;
            let salt = SaltString::encode_b64(&bytes)
                .map_err(|_| ApiError::internal("Unable to encode password salt"))?;
            Argon2::default()
                .hash_password(password.as_bytes(), &salt)
                .map(|h| h.to_string())
                .map_err(|_| ApiError::internal("Unable to hash password"))
        }
    })
    .await
    .map_err(|_| ApiError::internal("Password worker failed"))?
}
pub async fn verify(app: &App, headers: &HeaderMap, hash: Option<String>) -> Result<(), ApiError> {
    if let Some(hash) = hash {
        let password = credential(headers)?.ok_or_else(denied)?;
        work(app, password, Some(hash)).await?;
    }
    Ok(())
}
pub async fn authorize(
    app: &App,
    path: &str,
    headers: &HeaderMap,
) -> Result<Option<String>, ApiError> {
    let hash = app.protections.lock().unwrap().get(path).cloned();
    verify(app, headers, hash.clone()).await?;
    Ok(hash)
}
pub async fn for_upload(
    app: &App,
    headers: &HeaderMap,
    existing: &Option<String>,
) -> Result<Option<String>, ApiError> {
    if existing.is_some() {
        return Ok(existing.clone());
    }
    match credential(headers)? {
        Some(password) => Ok(Some(work(app, password, None).await?)),
        None => Ok(None),
    }
}

/// A public, unguessable locator derived from a randomly salted verifier.
/// The verifier itself and the original path never appear in listings.
pub fn location(path: &str, hash: &str) -> String {
    use sha2::{Digest, Sha256};
    let mut digest = Sha256::new();
    digest.update(hash.as_bytes());
    digest.update([0]);
    digest.update(path.as_bytes());
    format!("/.protected/{}", hex::encode(digest.finalize()))
}
