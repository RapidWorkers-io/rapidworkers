#![doc = include_str!("../README.md")]
#![forbid(unsafe_code)]
#![warn(missing_docs)]

mod client;
mod error;
// Written by scripts/gen_rust.py, which owns its layout
#[rustfmt::skip]
mod generated;

pub use client::{
    Client, ClientBuilder, API_KEYS_URL, API_KEY_ENV, BASE_URL_ENV, DEFAULT_MAX_RETRIES,
    DEFAULT_TIMEOUT, PROTOCOL_VERSION, VERSION,
};
pub use error::{Error, Result};
pub use generated::*;
