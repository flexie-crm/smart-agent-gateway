//! The SAG inference node.
//!
//! One machine that holds models and answers questions with them. To the
//! orchestrator it is a vendor row and its models are model rows (KB/35), which
//! is the whole trick: nothing above the adapter changes, because this node
//! serves the same surface every hosted vendor serves.
//!
//! What is here that a hosted vendor does not have is a **control plane**, and
//! that is what most of this crate is. The node owns its disk, so it is the one
//! that can say what it holds, what a download would cost, whether there is room
//! for it, and which models are in memory right now. The orchestrator asks and
//! records; it never reaches past this into the storage, and this never reaches
//! into the database.
//!
//! Read in this order: [`config`] for what a node is told, [`catalog`] for what
//! is on the disk, [`hub`] and [`pull`] for how it got there, [`engine`] for the
//! seam to the thing that runs it, and [`api`] for the surface all of that is
//! served through.

pub mod api;
pub mod catalog;
pub mod config;
pub mod engine;
pub mod error;
pub mod form;
pub mod hub;
pub mod join;
pub mod machine;
pub mod pull;
pub mod tls;
