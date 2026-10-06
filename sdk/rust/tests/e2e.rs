//! End to end against a real sandbox: BOX_E2E=<sandbox> cargo test.
//! Needs a built sandbox and a running (or startable) box server.

use sdbox::{Client, Command, Error};

#[test]
fn against_a_real_sandbox() {
    let Ok(name) = std::env::var("BOX_E2E") else {
        eprintln!("skipped: set BOX_E2E=<sandbox>");
        return;
    };
    let mut c = Client::new().unwrap();
    if let Ok(bin) = std::env::var("BOX_BIN") {
        c = c.with_binary(bin);
    }
    assert!(matches!(
        c.sandbox("no-such-sandbox").run("true"),
        Err(Error::NotFound(_))
    ));

    let sb = c.sandbox(&name);
    sb.up().unwrap();
    let out = sb
        .exec(Command::sh("echo out; echo err >&2; exit 3"))
        .unwrap();
    assert_eq!(
        (
            out.exit_code,
            out.stdout_text().as_str(),
            out.stderr_text().as_str()
        ),
        (3, "out\n", "err\n")
    );
    assert_eq!(
        sb.exec(Command::new(["wc", "-c"]).stdin(vec![0u8, 1, 255]))
            .unwrap()
            .stdout_text()
            .trim(),
        "3"
    );
    sb.write_file("/data/rs.txt", "hi $HOME 'q'\n").unwrap();
    assert_eq!(sb.read_file("/data/rs.txt").unwrap(), b"hi $HOME 'q'\n");
    let t = sb.exec(Command::sh("sleep 5").timeout(1)).unwrap();
    assert_eq!((t.exit_code, t.timed_out), (124, true));
    sb.down().unwrap();

    let trial = sb.fork("rs-trial").unwrap();
    trial
        .run("echo changed > /data/rs.txt; echo new > /data/added")
        .unwrap()
        .check()
        .unwrap();
    let mut diff = trial.diff().unwrap();
    diff.sort_by(|a, b| a.path.cmp(&b.path));
    assert_eq!(
        diff.iter()
            .map(|c| format!("{} {}", c.kind, c.path))
            .collect::<Vec<_>>(),
        ["A added", "M rs.txt"]
    );
    assert_eq!(trial.apply().unwrap(), 2);
    assert_eq!(
        sb.run("cat /data/rs.txt").unwrap().stdout_text(),
        "changed\n"
    );
    eprintln!("e2e ok");
}
