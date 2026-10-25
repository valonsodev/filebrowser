use crate::{
    filesystem::new_id,
    state::{App, Config},
    web,
};
use axum::{
    body::{Body, to_bytes},
    extract::State,
    http::{Request, StatusCode},
};
use std::{path::PathBuf, sync::Arc};

struct Fixture(PathBuf);
impl Fixture {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!("filebrowser-test-{}", new_id().unwrap()));
        std::fs::create_dir(&path).unwrap();
        Self(path)
    }
    fn app(&self) -> Arc<App> {
        App::new(
            Config {
                upload: true,
                ..Config::default()
            },
            &self.0,
        )
        .unwrap()
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}
async fn request(
    app: &Arc<App>,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &str,
) -> (StatusCode, axum::http::HeaderMap, String) {
    let mut req = Request::builder()
        .method(method)
        .uri(path)
        .header("host", "localhost");
    for (key, value) in headers {
        req = req.header(*key, *value);
    }
    let response = web::handler(
        State(app.clone()),
        req.body(Body::from(body.to_owned())).unwrap(),
    )
    .await;
    let (parts, body) = response.into_parts();
    assert_eq!(parts.headers["cache-control"], "no-store");
    (
        parts.status,
        parts.headers,
        String::from_utf8(to_bytes(body, 1024 * 1024).await.unwrap().to_vec()).unwrap(),
    )
}
const PASSWORD: (&str, &str) = ("File-Password", "test password never store plaintext");

#[tokio::test]
async fn protected_files_require_password_for_every_access_and_survive_restart() {
    let fixture = Fixture::new();
    let app = fixture.app();
    let (status, headers, _) = request(
        &app,
        "PUT",
        "/secret-finances.txt",
        &[PASSWORD],
        "private bytes",
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let alias = headers["file-location"].to_str().unwrap().to_owned();
    assert!(alias.starts_with("/.protected/"));
    let metadata =
        std::fs::read_to_string(fixture.0.join("files/.filebrowser/passwords.json")).unwrap();
    assert!(metadata.contains("$argon2id$"));
    assert!(!metadata.contains(PASSWORD.1));
    for headers in [
        vec![],
        vec![("File-Password", "wrong")],
        vec![("Range", "bytes=0-1")],
        vec![("If-None-Match", "*")],
    ] {
        let (status, headers, body) =
            request(&app, "GET", "/secret-finances.txt", &headers, "").await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        assert!(!headers.contains_key("content-disposition"));
        assert!(!body.contains("private bytes"));
    }
    for method in ["HEAD", "DELETE", "PUT", "PATCH"] {
        let path = if method == "PATCH" {
            "/secret-finances.txt?temporary=true"
        } else {
            "/secret-finances.txt"
        };
        let (status, _, body) = request(&app, method, path, &[], "malicious replacement").await;
        assert_eq!(status, StatusCode::UNAUTHORIZED, "{method}");
        if method == "HEAD" {
            assert!(body.is_empty());
        }
    }
    for headers in [
        vec![],
        vec![("Accept", "text/markdown")],
        vec![("User-Agent", "curl/8")],
    ] {
        let (status, _, listing) = request(&app, "GET", "/", &headers, "").await;
        assert_eq!(status, StatusCode::OK);
        assert!(listing.contains(&alias));
        assert!(listing.contains("Protected file"));
        assert!(!listing.contains("secret-finances"));
        assert!(!listing.contains("private bytes"));
        assert!(!listing.contains(PASSWORD.1));
    }
    let (status, _, body) = request(&app, "GET", &alias, &[("Accept", "text/html")], "").await;
    assert_eq!(status, StatusCode::OK);
    assert!(body.contains("type=\"password\""));
    assert!(!body.contains("secret-finances"));
    let (status, headers, body) =
        request(&app, "GET", &alias, &[PASSWORD, ("Range", "bytes=0-6")], "").await;
    assert_eq!(status, StatusCode::PARTIAL_CONTENT);
    assert_eq!(body, "private");
    assert!(
        headers["content-disposition"]
            .to_str()
            .unwrap()
            .contains("secret-finances.txt")
    );
    drop(app);
    let app = fixture.app();
    assert_eq!(
        request(&app, "GET", &alias, &[], "").await.0,
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(
        request(&app, "GET", &alias, &[PASSWORD], "").await.2,
        "private bytes"
    );
    assert_eq!(
        request(
            &app,
            "PUT",
            "/secret-finances.txt",
            &[PASSWORD],
            "replacement"
        )
        .await
        .0,
        StatusCode::CREATED
    );
    assert_eq!(
        request(&app, "GET", &alias, &[PASSWORD], "").await.2,
        "replacement"
    );
    assert_eq!(
        request(&app, "DELETE", &alias, &[PASSWORD], "").await.0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        request(&app, "GET", &alias, &[PASSWORD], "").await.0,
        StatusCode::NOT_FOUND
    );
}

#[tokio::test]
async fn protected_resumable_uploads_authenticate_discovery_and_resources() {
    let fixture = Fixture::new();
    let app = fixture.app();
    let headers = [
        PASSWORD,
        ("Upload-Complete", "?0"),
        ("Upload-Length", "5"),
        ("Upload-ID", "12345678901234567890123456789012"),
    ];
    let (status, result, _) =
        request(&app, "POST", "/hidden.txt?temporary=true", &headers, "").await;
    assert_eq!(status, StatusCode::CREATED);
    let location = result["location"].to_str().unwrap().to_owned();
    for method in ["HEAD", "DELETE", "PATCH"] {
        assert_eq!(
            request(&app, method, &location, &[], "").await.0,
            StatusCode::UNAUTHORIZED
        );
    }
    assert_eq!(
        request(
            &app,
            "POST",
            "/hidden.txt?temporary=true",
            &headers[1..],
            ""
        )
        .await
        .0,
        StatusCode::UNAUTHORIZED
    );
    let (status, result, _) =
        request(&app, "POST", "/hidden.txt?temporary=true", &headers, "").await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(result["location"], location);
    drop(app);
    let app = fixture.app();
    assert_eq!(
        request(&app, "HEAD", &location, &[], "").await.0,
        StatusCode::UNAUTHORIZED
    );
    let patch = [
        PASSWORD,
        ("Content-Type", "application/partial-upload"),
        ("Upload-Complete", "?1"),
        ("Upload-Offset", "0"),
    ];
    let (status, headers, _) = request(&app, "PATCH", &location, &patch, "hello").await;
    assert_eq!(status, StatusCode::CREATED);
    assert!(headers.contains_key("file-expires-at"));
    let alias = headers["file-location"].to_str().unwrap();
    assert_eq!(
        request(&app, "GET", alias, &[], "").await.0,
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(
        request(&app, "GET", alias, &[PASSWORD], "").await.2,
        "hello"
    );
    let on_disk = std::fs::read_to_string(
        fixture
            .0
            .join(".uploads")
            .join(location.rsplit('/').next().unwrap().to_owned() + ".json"),
    )
    .unwrap();
    assert!(!on_disk.contains(PASSWORD.1));
    assert!(on_disk.contains("$argon2id$"));
}

#[tokio::test]
async fn stale_public_upload_cannot_replace_a_newly_protected_file() {
    let fixture = Fixture::new();
    let app = fixture.app();
    let (_, headers, _) = request(
        &app,
        "POST",
        "/race.txt",
        &[("Upload-Complete", "?0"), ("Upload-Length", "3")],
        "",
    )
    .await;
    let location = headers["location"].to_str().unwrap();
    assert_eq!(
        request(&app, "PUT", "/race.txt", &[PASSWORD], "secure")
            .await
            .0,
        StatusCode::CREATED
    );
    let patch = [
        ("Content-Type", "application/partial-upload"),
        ("Upload-Complete", "?1"),
        ("Upload-Offset", "0"),
    ];
    assert_eq!(
        request(&app, "PATCH", location, &patch, "bad").await.0,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        request(&app, "GET", "/race.txt", &[PASSWORD], "").await.2,
        "secure"
    );
}

#[tokio::test]
async fn duplicate_or_invalid_password_headers_fail_and_public_uploads_stay_public() {
    let fixture = Fixture::new();
    let app = fixture.app();
    assert_eq!(
        request(&app, "PUT", "/bad.txt", &[("File-Password", "")], "x")
            .await
            .0,
        StatusCode::BAD_REQUEST
    );
    assert_eq!(
        request(&app, "PUT", "/bad.txt", &[PASSWORD, PASSWORD], "x")
            .await
            .0,
        StatusCode::BAD_REQUEST
    );
    assert_eq!(
        request(&app, "PUT", "/public.txt", &[], "public").await.0,
        StatusCode::CREATED
    );
    assert_eq!(
        request(&app, "GET", "/public.txt", &[], "").await.2,
        "public"
    );
    assert_eq!(
        request(&app, "GET", "/.filebrowser/passwords.json", &[PASSWORD], "")
            .await
            .0,
        StatusCode::NOT_FOUND
    );
    assert_eq!(
        request(&app, "GET", "/%2e%2e/secret", &[PASSWORD], "")
            .await
            .0,
        StatusCode::FORBIDDEN
    );
}

#[tokio::test]
async fn protected_expiry_removes_the_file_and_opaque_link() {
    let fixture = Fixture::new();
    let app = fixture.app();
    let (status, headers, _) = request(
        &app,
        "PUT",
        "/expires.txt?temporary=true",
        &[PASSWORD],
        "expire me",
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let alias = headers["file-location"].to_str().unwrap();
    app.actions
        .lock()
        .unwrap()
        .get_mut("expires.txt")
        .unwrap()
        .expires_at = chrono::Utc::now() - chrono::Duration::seconds(1);
    app.expire_files().unwrap();
    assert_eq!(
        request(&app, "HEAD", alias, &[], "").await.0,
        StatusCode::NOT_FOUND
    );
    assert!(!app.protections.lock().unwrap().contains_key("expires.txt"));
}

#[tokio::test]
async fn three_parallel_protected_uploads_can_queue_password_work() {
    let fixture = Fixture::new();
    let app = fixture.app();
    let (a, b, c) = tokio::join!(
        request(&app, "PUT", "/one.txt", &[PASSWORD], "one"),
        request(&app, "PUT", "/two.txt", &[PASSWORD], "two"),
        request(&app, "PUT", "/three.txt", &[PASSWORD], "three"),
    );
    for result in [a, b, c] {
        assert_eq!(result.0, StatusCode::CREATED);
    }
}

#[test]
fn corrupted_protection_metadata_prevents_startup() {
    let fixture = Fixture::new();
    let app = fixture.app();
    app.files
        .subdir(crate::filesystem::PRIVATE, true)
        .unwrap()
        .atomic_write("passwords.json", b"{broken")
        .unwrap();
    drop(app);
    assert!(App::new(Config::default(), &fixture.0).is_err());
}
