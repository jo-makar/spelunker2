mod browser;

use browser::Browser;

use std::thread;
use std::time::Duration;

fn main() {
    env_logger::init();

    let mut browser = Browser::new().unwrap();

    // FIXME STOPPED
    browser.goto("https://www.google.com", Some(Duration::from_secs(30)), None).unwrap();
    thread::sleep(Duration::from_secs(5));
}
