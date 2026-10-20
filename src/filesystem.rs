//! All user paths are resolved component by component from an open directory.
//! O_NOFOLLOW on every open prevents aliases and symlink replacement races.
use rustix::fs::{self, AtFlags, Mode, OFlags};
use std::{
    fs::File,
    io::{self, Read, Write},
    path::Path,
};

pub const PRIVATE: &str = ".filebrowser";

pub fn user_path(path: &str) -> io::Result<String> {
    let rel = path.strip_prefix('/').unwrap_or(path);
    let rel = rel.strip_suffix('/').unwrap_or(rel);
    if rel.is_empty() {
        return Ok(".".into());
    }
    if rel.contains(['\\', '\0'])
        || rel
            .split('/')
            .any(|p| matches!(p, "" | "." | ".." | PRIVATE))
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "invalid file path",
        ));
    }
    Ok(rel.into())
}

#[derive(Debug)]
pub struct Root {
    dir: File,
}
impl Root {
    pub fn open(path: impl AsRef<Path>) -> io::Result<Self> {
        let fd = fs::open(
            path.as_ref(),
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        Ok(Self { dir: fd.into() })
    }
    pub fn subdir(&self, name: &str, create: bool) -> io::Result<Self> {
        if create {
            match fs::mkdirat(&self.dir, name, Mode::from_raw_mode(0o700)) {
                Ok(()) | Err(rustix::io::Errno::EXIST) => (),
                Err(e) => return Err(e.into()),
            }
        }
        Ok(Self {
            dir: self.open_file(name, OFlags::RDONLY | OFlags::DIRECTORY, 0)?,
        })
    }
    fn parent(&self, path: &str, create: bool) -> io::Result<(File, String)> {
        let mut dir = self.dir.try_clone()?;
        let parts: Vec<_> = path.split('/').collect();
        if parts.iter().any(|p| {
            p.is_empty() || *p == ".." || (*p == "." && path != ".") || p.contains(['\\', '\0'])
        }) {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "invalid file path",
            ));
        }
        for part in &parts[..parts.len() - 1] {
            if create {
                match fs::mkdirat(&dir, *part, Mode::from_raw_mode(0o755)) {
                    Ok(()) | Err(rustix::io::Errno::EXIST) => (),
                    Err(e) => return Err(e.into()),
                }
            }
            dir = fs::openat(
                &dir,
                *part,
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )?
            .into();
        }
        Ok((dir, parts[parts.len() - 1].to_owned()))
    }
    pub fn validate_existing(&self, path: &str) -> io::Result<()> {
        let mut dir = self.dir.try_clone()?;
        let parts: Vec<_> = path.split('/').collect();
        for (i, part) in parts.iter().enumerate() {
            let stat = match fs::statat(&dir, *part, AtFlags::SYMLINK_NOFOLLOW) {
                Ok(s) => s,
                Err(rustix::io::Errno::NOENT) => return Ok(()),
                Err(e) => return Err(e.into()),
            };
            if fs::FileType::from_raw_mode(stat.st_mode) == fs::FileType::Symlink {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "symbolic links are not accessible",
                ));
            }
            if i + 1 < parts.len() {
                dir = fs::openat(
                    &dir,
                    *part,
                    OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                    Mode::empty(),
                )?
                .into();
            }
        }
        Ok(())
    }
    pub fn open_file(&self, path: &str, flags: OFlags, mode: u32) -> io::Result<File> {
        let (parent, name) = self.parent(path, false)?;
        let file: File = fs::openat(
            &parent,
            name.as_str(),
            flags | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
            Mode::from_raw_mode(mode),
        )?
        .into();
        let meta = file.metadata()?;
        if !meta.is_file() && !meta.is_dir() {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "only regular files and directories are accessible",
            ));
        }
        Ok(file)
    }
    pub fn read(&self, path: &str, limit: u64) -> io::Result<Vec<u8>> {
        let file = self.open_file(path, OFlags::RDONLY, 0)?;
        if !file.metadata()?.is_file() || file.metadata()?.len() > limit {
            return Err(io::Error::other("invalid state file"));
        }
        let mut bytes = Vec::new();
        file.take(limit + 1).read_to_end(&mut bytes)?;
        if bytes.len() as u64 > limit {
            return Err(io::Error::other("state file too large"));
        }
        Ok(bytes)
    }
    pub fn names(&self, path: &str) -> io::Result<Vec<String>> {
        let dir = self.open_file(path, OFlags::RDONLY | OFlags::DIRECTORY, 0)?;
        let mut names = Vec::new();
        for entry in fs::Dir::read_from(&dir)? {
            let entry = entry?;
            if let Ok(name) = entry.file_name().to_str()
                && name != "."
                && name != ".."
            {
                names.push(name.to_owned());
            }
        }
        names.sort();
        Ok(names)
    }
    pub fn mkdir(&self, path: &str) -> io::Result<()> {
        let (parent, name) = self.parent(path, false)?;
        fs::mkdirat(parent, name.as_str(), Mode::from_raw_mode(0o755))?;
        Ok(())
    }
    pub fn remove(&self, path: &str) -> io::Result<()> {
        let (parent, name) = self.parent(path, false)?;
        match fs::unlinkat(&parent, name.as_str(), AtFlags::empty()) {
            Ok(()) | Err(rustix::io::Errno::NOENT) => Ok(()),
            Err(e) => Err(e.into()),
        }
    }
    pub fn rename_to(
        &self,
        source: &str,
        root: &Root,
        target: &str,
        create: bool,
    ) -> io::Result<()> {
        let (parent, name) = root.parent(target, create)?;
        // Rename replaces the directory entry rather than following a hardlink.
        fs::renameat(&self.dir, source, &parent, name.as_str())?;
        parent.sync_all()?;
        self.dir.sync_all()
    }
    pub fn atomic_write(&self, path: &str, bytes: &[u8]) -> io::Result<()> {
        let name = format!(".state-{}", new_id()?);
        let result = (|| {
            let mut file =
                self.open_file(&name, OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL, 0o600)?;
            file.write_all(bytes)?;
            file.sync_all()?;
            self.rename_to(&name, self, path, false)
        })();
        let _ = self.remove(&name);
        result
    }
    pub fn sync(&self) -> io::Result<()> {
        self.dir.sync_all()
    }
}

pub fn new_id() -> io::Result<String> {
    let mut bytes = [0; 16];
    getrandom::fill(&mut bytes).map_err(io::Error::other)?;
    Ok(hex::encode(bytes))
}
