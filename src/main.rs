mod browser;

use browser::Browser;

use std::thread;
use std::time::Duration;

fn main() {
    env_logger::init();

    let browser = Browser::new().unwrap();

    // FIXME STOPPED
    thread::sleep(Duration::from_secs(5));
}
