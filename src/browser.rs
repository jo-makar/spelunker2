use nix::sys::signal;
use nix::unistd;

use std::env;
use std::error;
use std::fmt;
use std::process;
use std::thread;
use std::time;

pub struct Browser {
    child: process::Child
}

impl Browser {
    pub fn new() -> Result<Self, Box<dyn error::Error>> {
        let mut cmd = process::Command::new("/opt/google/chrome/chrome");
        cmd.args(&["-headless", "-remote-debugging-port=0"]);

        match env::var("HOME") {
            Ok(h) => { cmd.arg(format!("--user-data-dir={}/.config/google-chrome", h)); },
            Err(e) => {
                log::error!("error to determining HOME: {}", e);
                return Err(Box::new(e));
            }
        }

        cmd.stdin(process::Stdio::null())
           .stdout(process::Stdio::piped())
           .stderr(process::Stdio::piped());

        let child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                log::error!("error launching chrome: {}", e);
                return Err(Box::new(e));
            }
        };

        Ok(Self { child: child })
    }
}

impl Drop for Browser {
    fn drop(&mut self) {
        let mut alive = true;
        // FIXME Send a close command and test if alive with 15 sec timeout (as below)
        //       Generalize the test-if-alive code below in an internal function

        if !alive {
            return;
        }
        log::warn!("deadline reached waiting for Browser.close");

        match signal::kill(unistd::Pid::from_raw(self.child.id() as i32),
                           signal::Signal::SIGTERM) {
            Ok(()) => {
                for _ in 0..15 {
                    match self.child.try_wait() {
                        Ok(Some(_)) => {
                            alive = false;
                            break;
                        },
                        Ok(None) => {},
                        Err(e) => {
                            log::error!("try_wait error: {}", e);
                            break;
                        }
                    }
                    thread::sleep(time::Duration::from_secs(1));
                }
            },
            Err(e) => log::error!("error sending SIGTERM: {}", e)
        }

        if !alive {
            return;
        }
        log::warn!("deadline reached waiting for SIGTERM");

        if let Err(e) = self.child.kill() {
            log::error!("kill error: {}", e);
        }
    }
}

// FIXME Remove?
impl fmt::Display for Browser {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Browser {{ pid: {} }}", self.child.id())
    }
}
