use crate::{
    filesystem::{PRIVATE, Root, new_id, user_path},
    metrics::Metrics,
    upload::UploadEntry,
};
use chrono::{DateTime, Utc};
use rustix::fs::OFlags;
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    fs,
    io::{self, Write},
    os::unix::fs::DirBuilderExt,
    sync::{Arc, Mutex},
    time::Instant,
};

#[derive(Clone)]
pub struct Config {
    pub title: String,
    pub extra_headers: String,
    pub upload: bool,
    pub metrics: bool,
    pub address: String,
    pub port: u16,
}
impl Default for Config {
    fn default() -> Self {
        let boolean = |key: &str| {
            std::env::var(key)
                .is_ok_and(|s| matches!(s.as_str(), "1" | "true" | "TRUE" | "True" | "t" | "T"))
        };
        Self {
            title: std::env::var("TITLE").unwrap_or_else(|_| "File Server".into()),
            extra_headers: std::env::var("EXTRA_HEADERS").unwrap_or_default(),
            upload: boolean("ENABLE_UPLOAD"),
            metrics: boolean("ENABLE_METRICS"),
            address: "localhost".into(),
            port: 8000,
        }
    }
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Expiry {
    pub expires_at: DateTime<Utc>,
    pub modified: DateTime<Utc>,
    pub size: u64,
}
pub type Expirations = HashMap<String, Expiry>;
pub type UploadRef = Arc<UploadEntry>;
#[derive(Clone)]
pub struct DirSize {
    pub bytes: u64,
    pub complete: bool,
    pub modified: std::time::SystemTime,
}
pub struct App {
    pub config: Config,
    pub files: Root,
    pub store: Option<Root>,
    pub actions: Mutex<Expirations>,
    pub protections: Mutex<HashMap<String, String>>,
    pub password_work: crate::password::Work,
    pub uploads: Mutex<HashMap<String, UploadRef>>,
    pub discovery: tokio::sync::Mutex<()>,
    pub sizes: Mutex<HashMap<String, DirSize>>,
    pub metrics: Metrics,
}
impl App {
    pub fn new(config: Config, directory: &std::path::Path) -> io::Result<Arc<Self>> {
        fs::create_dir_all(directory.join("files"))?;
        let files = Root::open(directory.join("files"))?;
        let expirations = match files.subdir(PRIVATE, false) {
            Ok(meta) => match meta.read("expirations.json", 64 * 1024 * 1024) {
                Ok(data) => {
                    serde_json::from_slice::<Option<Expirations>>(&data)?.unwrap_or_default()
                }
                Err(e) if e.kind() == io::ErrorKind::NotFound => HashMap::new(),
                Err(e) => return Err(e),
            },
            Err(e) if e.kind() == io::ErrorKind::NotFound => HashMap::new(),
            Err(e) => return Err(e),
        };
        // A malformed verifier file prevents startup rather than exposing protected data.
        let protections = match files.subdir(PRIVATE, false) {
            Ok(meta) => match meta.read("passwords.json", 64 * 1024 * 1024) {
                Ok(data) => serde_json::from_slice::<HashMap<String, String>>(&data)?,
                Err(e) if e.kind() == io::ErrorKind::NotFound => HashMap::new(),
                Err(e) => return Err(e),
            },
            Err(e) if e.kind() == io::ErrorKind::NotFound => HashMap::new(),
            Err(e) => return Err(e),
        };
        let store = if config.upload {
            fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(directory.join(".uploads"))?;
            let root = Root::open(directory.join(".uploads"))?;
            let probe = format!(".write-probe-{}", new_id()?);
            root.open_file(
                &probe,
                OFlags::CREATE | OFlags::EXCL | OFlags::WRONLY,
                0o600,
            )?;
            root.remove(&probe)?;
            Some(root)
        } else {
            None
        };
        let metrics = Metrics::new(config.upload);
        let app = Arc::new(Self {
            config,
            files,
            store,
            actions: Mutex::new(expirations),
            protections: Mutex::new(protections),
            password_work: Default::default(),
            uploads: Mutex::new(HashMap::new()),
            discovery: tokio::sync::Mutex::new(()),
            sizes: Mutex::new(HashMap::new()),
            metrics,
        });
        app.expire_files()?;
        if app.config.upload {
            app.restore_uploads()?;
        }
        Ok(app)
    }
    pub fn store(&self) -> &Root {
        self.store.as_ref().expect("uploads enabled")
    }
    pub fn save_expirations(&self, entries: &Expirations) -> io::Result<()> {
        self.files
            .subdir(PRIVATE, true)?
            .atomic_write("expirations.json", &serde_json::to_vec(entries)?)
    }
    pub fn save_protections(&self, entries: &HashMap<String, String>) -> io::Result<()> {
        self.files
            .subdir(PRIVATE, true)?
            .atomic_write("passwords.json", &serde_json::to_vec(entries)?)
    }
    pub fn invalidate_sizes(&self, target: &str) {
        let mut sizes = self.sizes.lock().unwrap();
        sizes.remove(".");
        let mut path = target;
        while let Some((parent, _)) = path.rsplit_once('/') {
            sizes.remove(parent);
            path = parent;
        }
    }
    pub fn expire_files(&self) -> io::Result<()> {
        let mut entries = self.actions.lock().unwrap();
        let mut protections = self.protections.lock().unwrap();
        let mut protections_changed = false;
        let mut changed = false;
        let mut remove = Vec::new();
        for (path, expiry) in entries.iter() {
            if expiry.expires_at > Utc::now() {
                continue;
            }
            if user_path(path).ok().as_deref() != Some(path.as_str()) || path == "." {
                remove.push(path.clone());
                continue;
            }
            match self.files.open_file(path, OFlags::RDONLY, 0) {
                Ok(file) => {
                    let meta = file.metadata()?;
                    if meta.is_file()
                        && meta.len() == expiry.size
                        && DateTime::<Utc>::from(meta.modified()?) == expiry.modified
                    {
                        if let Err(e) = self.files.remove(path) {
                            eprintln!("Expiring {path}: {e}");
                            continue;
                        }
                        protections_changed |= protections.remove(path).is_some();
                        self.invalidate_sizes(path);
                    }
                    remove.push(path.clone());
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        io::ErrorKind::NotFound | io::ErrorKind::PermissionDenied
                    ) || e.raw_os_error() == Some(40)
                        || e.raw_os_error() == Some(20) =>
                {
                    remove.push(path.clone())
                }
                Err(e) => eprintln!("Expiring {path}: {e}"),
            }
        }
        for path in remove {
            entries.remove(&path);
            changed = true;
        }
        if protections_changed {
            self.save_protections(&protections)?;
        }
        if changed {
            self.save_expirations(&entries)?;
        }
        Ok(())
    }
    /// Caller does not hold actions. The lock covers installation and expiry state only.
    pub fn install(
        &self,
        source: &str,
        target: &str,
        temporary: bool,
        password_hash: Option<&str>,
        expected: Option<&str>,
    ) -> io::Result<i64> {
        if user_path(target).ok().as_deref() != Some(target) || target == "." {
            return Err(io::Error::other("invalid upload target"));
        }
        let mut entries = self.actions.lock().unwrap();
        let mut protections = self.protections.lock().unwrap();
        if protections.get(target).map(String::as_str) != expected {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "File protection changed; restart the upload",
            ));
        }
        self.files.validate_existing(target)?;
        let meta = self.files.subdir(PRIVATE, true)?;
        let stage = format!("upload-{}", new_id()?);
        let result = (|| {
            let mut output = meta.open_file(
                &stage,
                OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL,
                if password_hash.is_some() {
                    0o600
                } else {
                    0o644
                },
            )?;
            let mut input = self.store().open_file(source, OFlags::RDONLY, 0)?;
            io::copy(&mut input, &mut output)?;
            output.flush()?;
            output.sync_all()?;
            let info = output.metadata()?;
            let old = entries.get(target).cloned();
            let expires_at = if temporary {
                Utc::now() + chrono::Duration::minutes(5)
            } else {
                DateTime::UNIX_EPOCH
            };
            if temporary {
                entries.insert(
                    target.into(),
                    Expiry {
                        expires_at,
                        modified: info.modified()?.into(),
                        size: info.len(),
                    },
                );
            } else {
                entries.remove(target);
            }
            // Persist protection before publishing bytes. On I/O failure retain it:
            // rename may already have succeeded before a directory sync fails.
            if let Some(hash) = password_hash {
                protections.insert(target.into(), hash.into());
                self.save_protections(&protections)?;
            }
            let persist = temporary || old.is_some();
            if let Err(error) = (|| {
                if persist {
                    self.save_expirations(&entries)?;
                }
                self.files.validate_existing(target)?;
                meta.rename_to(&stage, &self.files, target, true)
            })() {
                if let Some(old) = old {
                    entries.insert(target.into(), old);
                } else {
                    entries.remove(target);
                }
                if persist {
                    self.save_expirations(&entries)?;
                }
                return Err(error);
            }
            self.invalidate_sizes(target);
            let _ = self.store().remove(source);
            Ok(if temporary { expires_at.timestamp() } else { 0 })
        })();
        let _ = meta.remove(&stage);
        result
    }
    pub fn dir_size(&self, path: &str, modified: std::time::SystemTime) -> DirSize {
        let protections = self.protections.lock().unwrap();
        if let Some(cached) = self
            .sizes
            .lock()
            .unwrap()
            .get(path)
            .filter(|c| c.modified == modified)
            .cloned()
        {
            return cached;
        }
        let deadline = Instant::now() + std::time::Duration::from_millis(100);
        let mut pending = vec![path.to_owned()];
        let mut result = DirSize {
            bytes: 0,
            complete: true,
            modified,
        };
        'scan: while let Some(path) = pending.pop() {
            if let Ok(names) = self.files.names(&path) {
                for name in names {
                    if Instant::now() >= deadline {
                        result.complete = false;
                        break 'scan;
                    }
                    if name == PRIVATE {
                        continue;
                    }
                    let child = if path == "." {
                        name
                    } else {
                        format!("{path}/{name}")
                    };
                    if protections.contains_key(&child) {
                        continue;
                    }
                    if let Ok(file) = self.files.open_file(&child, OFlags::RDONLY, 0)
                        && let Ok(info) = file.metadata()
                    {
                        if info.is_dir() {
                            pending.push(child);
                        } else {
                            result.bytes = result.bytes.saturating_add(info.len());
                        }
                    }
                }
            }
        }
        self.sizes
            .lock()
            .unwrap()
            .insert(path.into(), result.clone());
        result
    }
}
