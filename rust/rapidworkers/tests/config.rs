//! Configuration from arguments and the environment.
//!
//! Kept in its own test binary with a single test: it changes process environment variables,
//! which must not race with other tests.

use std::time::Duration;

use rapidworkers::{Client, Error};

#[test]
fn configuration_from_arguments_and_environment() {
    std::env::remove_var("RAPIDWORKERS_API_KEY");
    std::env::remove_var("RAPIDWORKERS_MCP_URL");

    let error = Client::from_env().unwrap_err();
    assert!(matches!(error, Error::Configuration(_)));
    assert!(error.to_string().contains("api-management"));
    assert!(Client::new("").is_err());
    assert!(Client::new("  ").is_err());
    assert!(matches!(
        Client::new("bad\nkey").unwrap_err(),
        Error::Configuration(_)
    ));

    let client = Client::new("secret-key-123").unwrap();
    assert_eq!(client.base_url(), rapidworkers::SERVER_URL);
    assert_eq!(client.timeout(), Duration::from_secs(60));
    assert_eq!(client.max_retries(), 2);
    assert!(!format!("{client:?}").contains("secret-key-123"));
    let builder = Client::builder().api_key("secret-key-123");
    assert!(!format!("{builder:?}").contains("secret-key-123"));

    std::env::set_var("RAPIDWORKERS_API_KEY", "envkeyenvkeyenvkey");
    std::env::set_var("RAPIDWORKERS_MCP_URL", " http://127.0.0.1:9/api/mcp/ ");
    let client = Client::from_env().unwrap();
    assert_eq!(client.base_url(), "http://127.0.0.1:9/api/mcp/");
    assert!(
        Client::new("").is_ok(),
        "an empty key falls back to the environment"
    );
    let client = Client::builder()
        .base_url("http://localhost:1/x/")
        .timeout(Duration::from_secs(5))
        .max_retries(0)
        .build()
        .unwrap();
    assert_eq!(client.base_url(), "http://localhost:1/x/");
    assert_eq!(client.timeout(), Duration::from_secs(5));
    assert_eq!(client.max_retries(), 0);

    std::env::set_var("RAPIDWORKERS_MCP_URL", "not a url");
    assert!(matches!(
        Client::from_env().unwrap_err(),
        Error::Configuration(_)
    ));

    std::env::remove_var("RAPIDWORKERS_API_KEY");
    std::env::remove_var("RAPIDWORKERS_MCP_URL");
}
