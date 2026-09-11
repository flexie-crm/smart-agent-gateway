//! What this machine is, and whether a model will fit on it.
//!
//! This is the half of "approval must equal success" that the orchestrator
//! cannot do for itself. It has no idea how much memory this box has or how much
//! of the disk is left, so before an administrator commits to an hour of
//! downloading, the node is asked and the node answers.
//!
//! The answer is deliberately a verdict AND a reason, never a filtered list. A
//! model that will not fit is still shown, saying why: somebody looking for it
//! needs to know it exists and that this is the wrong machine for it, which is
//! a different thing from it not existing.

use serde::Serialize;

/// Free space is never spent down to the last byte: a download that fills a
/// disk takes the logs and everything else on it with it.
const DISK_HEADROOM_BYTES: u64 = 8 * 1024 * 1024 * 1024;

/// What the node reports about itself.
#[derive(Debug, Clone, Serialize)]
pub struct Machine {
    pub memory_total: u64,
    pub memory_free: u64,
    pub disk_total: u64,
    pub disk_free: u64,
    pub processors: usize,
}

/// Whether a set of weights can be taken, and what to say if not.
#[derive(Debug, Clone, Serialize)]
pub struct Verdict {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

impl Verdict {
    /// A verdict anything outside this module can build. The two here read the
    /// same way at the call site as the machine's own do, which is the point:
    /// whether a model fits and whether it may be fetched are the same kind of
    /// answer to the caller.
    pub fn allowed() -> Self {
        Self::yes()
    }

    pub fn refused(reason: impl Into<String>) -> Self {
        Self::no(reason)
    }

    fn yes() -> Self {
        Self {
            ok: true,
            reason: None,
        }
    }

    fn no(reason: impl Into<String>) -> Self {
        Self {
            ok: false,
            reason: Some(reason.into()),
        }
    }
}

impl Machine {
    /// Read the machine now. Called per request, because the answer is only
    /// worth anything if it is current: a cached figure would approve a download
    /// onto a disk another download has since filled.
    pub fn read(data_dir: &std::path::Path) -> Self {
        use sysinfo::{Disks, System};

        let mut system = System::new();
        system.refresh_memory();

        let disks = Disks::new_with_refreshed_list();
        // The disk the node writes to, which is the longest mount point that is
        // a prefix of the data directory. Picking the first disk instead would
        // report the root filesystem for a node whose models live on a mounted
        // volume, which is the case on every machine that has one.
        let mount = disks
            .iter()
            .filter(|d| data_dir.starts_with(d.mount_point()))
            .max_by_key(|d| d.mount_point().as_os_str().len());

        Self {
            memory_total: system.total_memory(),
            memory_free: system.available_memory(),
            disk_total: mount.map(|d| d.total_space()).unwrap_or(0),
            disk_free: mount.map(|d| d.available_space()).unwrap_or(0),
            processors: std::thread::available_parallelism()
                .map(|n| n.get())
                .unwrap_or(1),
        }
    }

    /// Whether these weights can be downloaded here.
    pub fn can_store(&self, size_bytes: u64) -> Verdict {
        // A disk we could not read is not a disk we should refuse on: reporting
        // zero free is what a container with an unusual mount looks like, and
        // refusing every pull on it would be worse than letting the download
        // fail honestly if it really is full.
        if self.disk_total == 0 {
            return Verdict::yes();
        }
        let needed = size_bytes.saturating_add(DISK_HEADROOM_BYTES);
        if self.disk_free >= needed {
            Verdict::yes()
        } else {
            Verdict::no(format!(
                "needs {} and this node has {} free",
                human(size_bytes),
                human(self.disk_free)
            ))
        }
    }

    /// Whether these weights are likely to load into memory here.
    ///
    /// Two different refusals, because they have two different answers. A model
    /// larger than the machine will never fit and the only remedy is a smaller
    /// model or a bigger machine. A model larger than what is FREE might fit
    /// perfectly well once something else is closed, or once it is compressed on
    /// load, and telling somebody that is worth more than a flat no.
    ///
    /// It used to compare against the total only, and said yes to a 6.4GB model
    /// on a 17GB machine with 2.9GB free. Somebody downloaded it on that promise
    /// and the engine refused it: "exceeds total capacity by 400MB". A verdict
    /// given before an hour of downloading has to be about the memory that is
    /// actually there.
    pub fn can_load(&self, size_bytes: u64) -> Verdict {
        if self.memory_total == 0 {
            return Verdict::yes();
        }
        if size_bytes > self.memory_total {
            return Verdict::no(format!(
                "needs about {} and this node only has {} in total",
                human(size_bytes),
                human(self.memory_total)
            ));
        }
        if size_bytes > self.memory_free {
            return Verdict::no(format!(
                "needs about {} and only {} is free at the moment: it will fit once something else \
                 is closed, or if it is compressed when loaded",
                human(size_bytes),
                human(self.memory_free)
            ));
        }
        Verdict::yes()
    }
}

/// Bytes as somebody would say them.
fn human(bytes: u64) -> String {
    const UNITS: [(&str, u64); 4] = [
        ("TB", 1_000_000_000_000),
        ("GB", 1_000_000_000),
        ("MB", 1_000_000),
        ("kB", 1_000),
    ];
    for (unit, scale) in UNITS {
        if bytes >= scale {
            return format!("{:.1} {unit}", bytes as f64 / scale as f64);
        }
    }
    format!("{bytes} bytes")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn machine(memory_total: u64, disk_free: u64) -> Machine {
        Machine {
            memory_total,
            memory_free: memory_total,
            disk_total: disk_free * 2 + 1,
            disk_free,
            processors: 8,
        }
    }

    const GB: u64 = 1_000_000_000;

    #[test]
    fn a_model_that_fits_with_room_to_spare_is_allowed() {
        let m = machine(64 * GB, 500 * GB);
        assert!(m.can_store(20 * GB).ok);
        assert!(m.can_load(20 * GB).ok);
    }

    #[test]
    fn a_model_larger_than_the_disk_is_refused_with_both_numbers() {
        let m = machine(64 * GB, 10 * GB);
        let verdict = m.can_store(40 * GB);
        assert!(!verdict.ok);
        let reason = verdict.reason.expect("refused without saying why");
        assert!(reason.contains("40.0 GB"), "{reason}");
        assert!(reason.contains("10.0 GB"), "{reason}");
    }

    #[test]
    fn the_disk_is_not_filled_to_the_last_byte() {
        // Exactly enough room for the weights and nothing else is not enough
        // room: the node still has to write logs on the way.
        let m = machine(64 * GB, 40 * GB);
        assert!(!m.can_store(40 * GB).ok, "the disk was spent down to zero");
        assert!(m.can_store(40 * GB - DISK_HEADROOM_BYTES).ok);
    }

    #[test]
    fn a_model_larger_than_the_machine_is_refused_outright() {
        let m = machine(16 * GB, 500 * GB);
        let verdict = m.can_load(70 * GB);
        assert!(!verdict.ok);
        let reason = verdict.reason.unwrap();
        assert!(reason.contains("16.0 GB"), "{reason}");
        assert!(reason.contains("in total"), "{reason}");
    }

    #[test]
    fn a_model_larger_than_what_is_free_says_it_might_fit_later() {
        // The one that cost somebody an hour: 6.4GB looked fine against 17GB of
        // total memory, and the engine refused it with 2.9GB actually free.
        // Refusing is right; refusing as though the machine were too small is
        // not, because closing a browser would fix it.
        let mut m = machine(17 * GB, 500 * GB);
        m.memory_free = 2_900_000_000;

        let verdict = m.can_load(6_430_000_000);
        assert!(!verdict.ok, "a model that does not fit was approved");
        let reason = verdict.reason.unwrap();
        assert!(reason.contains("2.9 GB"), "{reason}");
        assert!(reason.contains("free at the moment"), "{reason}");
        // And it must NOT read as a machine that is too small, because it is not.
        assert!(!reason.contains("in total"), "{reason}");
    }

    #[test]
    fn a_model_that_fits_in_free_memory_is_allowed() {
        let mut m = machine(17 * GB, 500 * GB);
        m.memory_free = 8 * GB;
        assert!(m.can_load(6 * GB).ok);
    }

    #[test]
    fn an_unreadable_disk_does_not_refuse_everything() {
        // A container with an unusual mount reports nothing. Refusing every pull
        // on it would be worse than letting the download fail honestly.
        let mut m = machine(64 * GB, 0);
        m.disk_total = 0;
        assert!(m.can_store(40 * GB).ok);
    }

    #[test]
    fn unreadable_memory_does_not_refuse_everything() {
        let m = machine(0, 500 * GB);
        assert!(m.can_load(40 * GB).ok);
    }

    #[test]
    fn sizes_read_the_way_a_person_says_them() {
        assert_eq!(human(0), "0 bytes");
        assert_eq!(human(999), "999 bytes");
        assert_eq!(human(1_500), "1.5 kB");
        assert_eq!(human(2_500_000), "2.5 MB");
        assert_eq!(human(70 * GB), "70.0 GB");
        assert_eq!(human(2_000_000_000_000), "2.0 TB");
    }

    #[test]
    fn a_refusal_names_no_part_of_the_stack() {
        let m = machine(16 * GB, GB);
        for verdict in [m.can_store(40 * GB), m.can_load(40 * GB)] {
            let reason = verdict.reason.unwrap().to_lowercase();
            for banned in ["vram", "gpu", "cuda", "ram", "mistral"] {
                assert!(!reason.contains(banned), "{reason:?} names {banned}");
            }
        }
    }

    #[test]
    fn reading_this_machine_answers_something_sane() {
        let m = Machine::read(std::path::Path::new("."));
        assert!(m.memory_total > 0, "this machine reports no memory");
        assert!(m.processors >= 1);
    }
}
