#[cfg(not(target_os = "linux"))]
compile_error!("filebrowser requires Linux for the rustix raw syscall backend");

mod download;
mod filesystem;
mod listing;
mod metrics;
mod state;
mod upload;
mod web;

use state::{App, Config};
use std::{error::Error, time::Duration};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt};

fn arguments() -> Result<(Config, bool), String> {
    let mut config = Config::default();
    let mut args = std::env::args().skip(1).peekable();
    let mut health = false;
    while let Some(arg) = args.next() {
        if arg == "health" {
            health = true;
            continue;
        }
        if matches!(arg.as_str(), "--help" | "-h" | "-help") {
            println!(
                "filebrowser [health] [--enable-upload] [--enable-metrics] [--address localhost] [--port 8000]\n\nEnvironment: TITLE, EXTRA_HEADERS, ENABLE_UPLOAD, ENABLE_METRICS\nFiles: ./files   Partial uploads: ./.uploads"
            );
            std::process::exit(0);
        }
        let (key, value) = arg
            .trim_start_matches('-')
            .split_once('=')
            .map_or((arg.trim_start_matches('-'), None), |(k, v)| (k, Some(v)));
        match key {
            "enable-upload" | "enable-metrics" => {
                let enabled = match value {
                    None | Some("true") => true,
                    Some("false") => false,
                    _ => return Err(format!("invalid value for {key}")),
                };
                if key == "enable-upload" {
                    config.upload |= enabled;
                } else {
                    config.metrics |= enabled;
                }
            }
            "address" | "port" => {
                let value = value
                    .map(str::to_owned)
                    .or_else(|| args.next())
                    .ok_or_else(|| format!("missing value for {key}"))?;
                if key == "address" {
                    config.address = value;
                } else {
                    config.port = value.parse().map_err(|_| "invalid port")?;
                }
            }
            _ => return Err(format!("unknown argument: {arg}")),
        }
    }
    Ok((config, health))
}
async fn health(config: &Config) -> Result<(), Box<dyn Error>> {
    tokio::time::timeout(Duration::from_secs(3), async {
        let mut stream =
            tokio::net::TcpStream::connect((config.address.as_str(), config.port)).await?;
        stream
            .write_all(
                format!(
                    "GET / HTTP/1.1\r\nHost: localhost:{}\r\nConnection: close\r\n\r\n",
                    config.port
                )
                .as_bytes(),
            )
            .await?;
        let mut status = String::new();
        tokio::io::BufReader::new(stream)
            .read_line(&mut status)
            .await?;
        if !status.starts_with("HTTP/1.1 2") {
            return Err(std::io::Error::other("unhealthy server"));
        }
        Ok(())
    })
    .await??;
    Ok(())
}
#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let (config, check) = arguments().map_err(std::io::Error::other)?;
    if check {
        return health(&config).await;
    }
    let current = std::env::current_dir()?;
    let app = tokio::task::spawn_blocking(move || App::new(config, &current)).await??;
    let listener =
        tokio::net::TcpListener::bind((app.config.address.as_str(), app.config.port)).await?;
    eprintln!("Server running at http://{}", listener.local_addr()?);
    eprintln!(
        "File uploads are {}; metrics are {}",
        if app.config.upload {
            "enabled"
        } else {
            "disabled"
        },
        if app.config.metrics {
            "enabled"
        } else {
            "disabled"
        }
    );
    let janitor_app = app.clone();
    let janitor = tokio::spawn(async move {
        let mut timer = tokio::time::interval(Duration::from_secs(1));
        loop {
            timer.tick().await;
            let app = janitor_app.clone();
            let _ = tokio::task::spawn_blocking(move || {
                if let Err(e) = app.expire_files() {
                    eprintln!("Expiring files: {e}");
                }
                if app.config.upload
                    && let Err(e) = app.expire_uploads()
                {
                    eprintln!("Expiring uploads: {e}");
                }
            })
            .await;
        }
    });
    let router = axum::Router::new().fallback(web::handler).with_state(app);
    axum::serve(listener, router)
        .with_graceful_shutdown(async {
            let mut term =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                    .expect("signal handler");
            tokio::select! { _ = tokio::signal::ctrl_c() => (), _ = term.recv() => () }
        })
        .await?;
    janitor.abort();
    Ok(())
}
