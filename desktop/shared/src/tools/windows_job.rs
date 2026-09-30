//! Ending a process and everything it started, on Windows.
//!
//! This is the platform's answer to what Unix does with a process GROUP and
//! `killpg`. A job is a set of processes the system enforces limits on
//! together: a child assigned to one cannot leave it, and every process IT
//! starts joins the same job. That is what makes "end everything it started" a
//! single call rather than a walk of the process tree that races whatever is
//! still spawning.
//!
//! TWO CALLERS, TWO POLICIES, ONE MECHANISM.
//!
//! `hold` is for the terminal: kill-on-close and nothing else. A terminal runs
//! builds and test suites, so a processor ceiling there would end the very work
//! it exists for.
//!
//! `bounded` is for a skill's script, which is a different thing: something the
//! model chose to run, held to the ceilings that file declares. It adds a
//! processor limit and a memory limit to the same object.
//!
//! WHAT A JOB CANNOT DO, so it is not looked for: there is no equivalent of
//! `RLIMIT_FSIZE`. A file-size ceiling is honestly absent on this platform
//! rather than pretended at.
//!
//! KILL_ON_JOB_CLOSE is what makes the handle worth holding: when this process
//! drops it, anything still inside goes. That covers the case no signal can,
//! which is this application dying while a command's children are running.

use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
use windows_sys::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, JobObjectExtendedLimitInformation,
    SetInformationJobObject, TerminateJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
    JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, JOB_OBJECT_LIMIT_PROCESS_MEMORY,
    JOB_OBJECT_LIMIT_PROCESS_TIME,
};

/// The ceilings a job may carry, where the caller wants them.
///
/// Absent for a terminal, present for a skill's script. Expressed as a type
/// rather than two booleans so a caller cannot ask for a limit and forget to
/// give it a value.
pub(crate) struct Ceilings {
    /// Processor seconds, per process in the job.
    pub cpu_seconds: u64,
    /// Commit limit in bytes, per process in the job.
    pub memory: u64,
}

/// A job, kept as an integer rather than as the pointer it is.
///
/// `HANDLE` is `*mut c_void`, which is not `Send`, and both callers hold this
/// across an await in a future that has to be. The integer is the same value
/// and costs one cast at each use.
pub(crate) struct Job(isize);

impl Job {
    /// hold puts a freshly spawned child in a job of its own, with no ceilings.
    ///
    /// For the terminal: what is wanted there is the TREE, not a limit.
    pub(crate) fn hold(child: &tokio::process::Child) -> Option<Self> {
        Self::create(child, None)
    }

    /// bounded is the same, carrying the caller's ceilings.
    pub(crate) fn bounded(child: &tokio::process::Child, ceilings: Ceilings) -> Option<Self> {
        Self::create(child, Some(ceilings))
    }

    /// Answers None if the system refuses any step, which leaves the command
    /// running unsupervised rather than not running at all.
    ///
    /// That is deliberate and it is the same decision `bound` makes on Unix,
    /// where every `setrlimit` failure is ignored: a ceiling the system will
    /// not set is not a reason to refuse to run somebody's command.
    fn create(child: &tokio::process::Child, ceilings: Option<Ceilings>) -> Option<Self> {
        let process = child.raw_handle()? as HANDLE;
        // SAFETY: null attributes and a null name ask for an unnamed job owned
        // by this process. The handle is ours until Drop closes it.
        let job = unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) };
        if job.is_null() {
            return None;
        }
        // Owned from here, so every path below closes it.
        let held = Self(job as isize);

        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        if let Some(ceilings) = ceilings {
            limits.BasicLimitInformation.LimitFlags |=
                JOB_OBJECT_LIMIT_PROCESS_TIME | JOB_OBJECT_LIMIT_PROCESS_MEMORY;
            // 100ns units, which is what this field counts in.
            limits.BasicLimitInformation.PerProcessUserTimeLimit =
                ceilings.cpu_seconds.saturating_mul(10_000_000) as i64;
            limits.ProcessMemoryLimit = ceilings.memory as usize;
        }

        // SAFETY: the pointer and the length describe the struct above, which
        // outlives the call.
        let set = unsafe {
            SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                std::ptr::addr_of!(limits).cast(),
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
        };
        if set == 0 {
            return None;
        }

        // Assigned AFTER the limits, so there is no moment where the child is
        // in a job that does not yet hold it.
        //
        // One gap this cannot close: a child that spawns something between
        // CreateProcess returning and this call. It is microseconds, and no
        // shell has finished starting by then; closing it properly needs
        // CREATE_SUSPENDED, and tokio exposes no way to resume the thread
        // afterwards.
        //
        // SAFETY: both handles are open and ours.
        if unsafe { AssignProcessToJobObject(job, process) } == 0 {
            return None;
        }
        Some(held)
    }

    /// end ends every process in the job at once.
    ///
    /// This is the call `killpg` makes on the other platform, and it is what
    /// reaches a server a command started: killing the shell alone leaves that
    /// server running AND holding the pipes the output is read from, so the
    /// read never ends and the call never returns.
    pub(crate) fn end(&self) {
        // SAFETY: the handle is ours and still open, which is what holding it
        // is for.
        unsafe { TerminateJobObject(self.0 as HANDLE, 1) };
    }
}

impl Drop for Job {
    fn drop(&mut self) {
        // KILL_ON_JOB_CLOSE means this ends anything still inside. That is the
        // orphan case: a command that started something and exited happily
        // leaves nothing behind on this platform.
        //
        // SAFETY: closed exactly once, here, by the owner.
        unsafe { CloseHandle(self.0 as HANDLE) };
    }
}
