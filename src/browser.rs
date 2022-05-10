use nix::sys::signal;
use nix::unistd;

use std::env;
use std::error;
use std::fmt;
use std::io::{self, BufRead};
use std::process;
use std::thread;
use std::time;

pub struct Browser {
    child: process::Child,
}

impl Browser {
    pub fn new() -> Result<Self, Box<dyn error::Error>> {
        let mut cmd = process::Command::new("/opt/google/chrome/chrome");
        cmd.args(&["--headless", "--remote-debugging-port=0"]);

        match env::var("HOME") {
            Ok(h) => { cmd.arg(format!("--user-data-dir={}/.config/google-chrome", h)); },
            Err(e) => {
                log::error!("error to determining HOME: {}", e);
                return Err(Box::new(e));
            }
        }

        cmd.stdin(process::Stdio::null())
           .stdout(process::Stdio::null())
           .stderr(process::Stdio::piped());

        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                log::error!("error launching chrome: {}", e);
                return Err(Box::new(e));
            }
        };

        let stderr = child.stderr.take().unwrap();

        // Ideally would unwrap stderr from the BufReader and TimeoutReader for later use in the thread below.
        // However (at least currently) only BufReader supports into_inner() to unwrap the underlying reader.
        let mut stderr_reader = io::BufReader::new(timeout_readwrite::TimeoutReader::new(stderr, time::Duration::from_secs(1)));

        let mut port: Option<u16> = None;
        {
            let re = regex::Regex::new(r"^DevTools listening on (ws://127.0.0.1:(\d+)/devtools/browser/[a-f0-9-]+)$").unwrap();

            for _ in 0..15 {
                let mut line = String::new();
                match stderr_reader.read_line(&mut line) {
                    Ok(_) => {
                        if line.ends_with('\n') {
                            line.pop();
                        }
                        if !line.is_empty() {
                            log::info!("stderr: {}", line);

                            if let Some(c) = re.captures(&line) {
                                log::info!("websocket url: {}", c.get(1).unwrap().as_str());

                                let s = c.get(2).unwrap().as_str();
                                match s.parse::<u16>() {
                                    Ok(p) => {
                                        port = Some(p);
                                        break;
                                    },
                                    Err(e) => log::error!("error parsing port {}: {}", s, e)
                                }
                            }
                        }
                    },
                    Err(e) if e.kind() == io::ErrorKind::TimedOut => {},
                    Err(e) => {
                        log::error!("error reading stderr: {}", e);
                        break;
                    }
                }
            }

            if let None = port {
                let s = "websocket url not found";
                log::error!("{}", s);
                return Err(string_error::static_err(s));
            }
        }
        let port = port.unwrap();

        let debugger_url: String;
        {
            #[derive(serde::Deserialize, Debug)]
            struct Config {
                #[serde(rename = "webSocketDebuggerUrl")]
                debugger_url: String,
            }

            match reqwest::blocking::get(format!("http://127.0.0.1:{}/json", port)) {
                Ok(r) if r.status() == reqwest::StatusCode::OK => {
                    match r.json::<Vec<Config>>() {
                        Ok(c) if c.len() == 1 => {
                            debugger_url = c[0].debugger_url.clone();
                            log::info!("debugger url: {}", debugger_url);
                        },
                        Ok(c) => {
                            let s = format!("unexpected json url response config count ({})", c.len());
                            log::error!("{}", s);
                            return Err(string_error::into_err(s));
                        },
                        Err(e) => {
                            log::error!("error parsing json url response: {}", e);
                            return Err(Box::new(e));
                        }
                    }
                },
                Ok(r) => {
                    let s = format!("{} status code from json url", r.status());
                    log::error!("{}", s);
                    return Err(string_error::into_err(s));
                },
                Err(e) => {
                    log::error!("error accessing json url: {}", e);
                    return Err(Box::new(e));
                }
            }
        }

        thread::spawn(move || {
            for line in stderr_reader.lines() {
                match line {
                    Ok(l) => {
                        if !l.is_empty() {
                            log::info!("stderr: {}", l);
                        }
                    },
                    Err(e) if e.kind() == io::ErrorKind::TimedOut => {},
                    Err(e) => {
                        log::error!("error reading stderr: {}", e);
                        break;
                    }
                }
            }
        });

        // FIXME STOPPED Open a websocket connection to the debugger url
        //               Then define a thread to read messages into a circular buffer
        //               Unlike the stderr thread will need to be joined when Browser is dropped

        Ok(Self {
            child: child,
        })
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

        match signal::kill(unistd::Pid::from_raw(self.child.id() as i32), signal::Signal::SIGTERM) {
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

impl fmt::Debug for Browser {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Browser {{ pid: {} }}", self.child.id())
    }
}
