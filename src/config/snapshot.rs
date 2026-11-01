use crate::{
    instructions::{self, File},
    paths::Paths,
};
use anyhow::{Context, Result, bail};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs, io,
    path::{Path, PathBuf},
};
const MAX_FILE: usize = 16 << 20;
const MAX_TOTAL: usize = 64 << 20;
const MAX_FILES: usize = 4096;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Entry {
    pub name: String,
    pub directory: bool,
}
pub trait Source {
    fn read_file(&self, path: &Path) -> Result<Vec<u8>>;
    fn read_dir(&self, path: &Path) -> Result<Vec<Entry>>;
}
pub struct Disk;
impl Source for Disk {
    fn read_file(&self, path: &Path) -> Result<Vec<u8>> {
        Ok(instructions::read_regular(path, MAX_FILE)?)
    }
    fn read_dir(&self, path: &Path) -> Result<Vec<Entry>> {
        let mut entries = Vec::new();
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            entries.push(Entry {
                name: entry.file_name().into_string().map_err(|_| {
                    io::Error::new(
                        io::ErrorKind::InvalidData,
                        "configuration name is not UTF-8",
                    )
                })?,
                directory: entry.file_type()?.is_dir(),
            });
        }
        entries.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(entries)
    }
}
/// The bytes accepted by project trust are the only bytes later loaders can read.
#[derive(Clone, Debug)]
pub struct Snapshot {
    pub dir: PathBuf,
    pub files: Vec<String>,
    pub hash: String,
    pub chain: Vec<File>,
    data: BTreeMap<String, Vec<u8>>,
}
impl Snapshot {
    pub fn take(dir: &Path, home: &Path) -> Result<Self> {
        let dir = crate::pathx::absolute(dir)?;
        let mut snapshot = Self {
            dir: dir.clone(),
            files: Vec::new(),
            hash: String::new(),
            chain: Vec::new(),
            data: BTreeMap::new(),
        };
        let parent = dir.join(".agents");
        match fs::symlink_metadata(&parent) {
            Ok(metadata) if !metadata.is_dir() => {
                bail!("{} is not a real directory", parent.display())
            }
            Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e.into()),
            _ => {}
        }
        let mut total = 0usize;
        for root in [Paths::project(&dir), Paths::project_skills(&dir)] {
            let mut stack = vec![root];
            while let Some(path) = stack.pop() {
                let metadata = match fs::symlink_metadata(&path) {
                    Ok(m) => m,
                    Err(e) if e.kind() == io::ErrorKind::NotFound => continue,
                    Err(e) => return Err(e.into()),
                };
                if metadata.is_dir() {
                    let mut children = fs::read_dir(&path)?
                        .map(|e| e.map(|e| e.path()))
                        .collect::<io::Result<Vec<_>>>()?;
                    children.sort();
                    stack.extend(children.into_iter().rev());
                } else {
                    snapshot.add(&path, &mut total)?;
                }
            }
        }
        for file in instructions::chain(&dir, home) {
            snapshot.add(&file.path, &mut total)?;
            let relative = snapshot.relative(&file.path)?;
            snapshot.chain.push(File {
                path: file.path,
                text: String::from_utf8_lossy(&snapshot.data[&relative]).into_owned(),
            });
        }
        for path in instructions::nested(&dir) {
            snapshot.add(&path, &mut total)?;
        }
        snapshot.files = snapshot.data.keys().cloned().collect();
        if !snapshot.files.is_empty() {
            let mut hash = Sha256::new();
            for (name, bytes) in &snapshot.data {
                hash.update(format!("{name}\0{}\0", bytes.len()));
                hash.update(bytes);
            }
            snapshot.hash = format!("{:x}", hash.finalize())[..32].to_owned();
        }
        Ok(snapshot)
    }
    fn relative(&self, path: &Path) -> Result<String> {
        crate::pathx::relative(&self.dir, path)
            .context("cannot make configuration path relative")?
            .into_os_string()
            .into_string()
            .map_err(|_| anyhow::anyhow!("configuration path is not UTF-8"))
    }
    fn add(&mut self, path: &Path, total: &mut usize) -> Result<()> {
        let relative = self.relative(path)?;
        if self.data.contains_key(&relative) {
            return Ok(());
        }
        if self.data.len() >= MAX_FILES {
            bail!("the project's configuration has more than {MAX_FILES} files");
        }
        let bytes = instructions::read_regular(path, MAX_FILE)?;
        *total += bytes.len();
        if *total > MAX_TOTAL {
            bail!(
                "the project's configuration is larger than {} MB",
                MAX_TOTAL >> 20
            );
        }
        self.data.insert(relative, bytes);
        Ok(())
    }
}
fn absent(path: &Path) -> anyhow::Error {
    io::Error::new(
        io::ErrorKind::NotFound,
        format!("{} is not in the configuration snapshot", path.display()),
    )
    .into()
}
impl Source for Snapshot {
    fn read_file(&self, path: &Path) -> Result<Vec<u8>> {
        self.data
            .get(&self.relative(path)?)
            .cloned()
            .ok_or_else(|| absent(path))
    }
    fn read_dir(&self, path: &Path) -> Result<Vec<Entry>> {
        let prefix = format!("{}/", self.relative(path)?);
        let mut entries = BTreeMap::new();
        for name in self.data.keys() {
            if let Some(tail) = name.strip_prefix(&prefix) {
                let (name, directory) = tail
                    .split_once('/')
                    .map_or((tail, false), |(name, _)| (name, true));
                entries.entry(name.to_owned()).or_insert(directory);
            }
        }
        if entries.is_empty() {
            return Err(absent(path));
        }
        Ok(entries
            .into_iter()
            .map(|(name, directory)| Entry { name, directory })
            .collect())
    }
}
