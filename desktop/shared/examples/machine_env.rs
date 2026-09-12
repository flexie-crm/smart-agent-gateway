//! Prints what this computer reports about itself, as the page sends it.
//!
//! It exists so the Go side can be driven by what THIS code actually produces
//! rather than by a hand-written copy of it. A test that invents the JSON
//! proves the two halves agree with the same assumption, which is the thing a
//! cross-language seam cannot check about itself.
//!
//!     cargo run -p sag-desktop --example machine_env

fn main() {
    let env = sag_desktop::environment::describe();
    match serde_json::to_string_pretty(&env) {
        Ok(json) => println!("{json}"),
        Err(err) => {
            eprintln!("the environment could not be written: {err}");
            std::process::exit(1);
        }
    }
}
