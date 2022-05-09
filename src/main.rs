mod browser;

use std::thread;
use std::time;

fn main() {
    env_logger::init();

    let browser = browser::Browser::new().unwrap();

    // FIXME STOPPED
    thread::sleep(time::Duration::from_secs(5));
}
