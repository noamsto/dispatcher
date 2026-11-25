use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::mpsc::Sender;
use std::thread;
use std::time::Duration;

use crossterm::event::{self, Event, KeyEvent, KeyEventKind};
use serde_json::Value;

use crate::model::{parse_doc, Snapshot};

pub enum Msg {
    Doc(Snapshot),
    Key(KeyEvent),
    Resize,
    SourceEnded,
}

/// Splits a stream of concatenated JSON documents (pretty-printed objects or one per line).
pub fn split_docs(text: &str) -> Vec<Result<Value, String>> {
    serde_json::Deserializer::from_str(text)
        .into_iter::<Value>()
        .map(|r| r.map_err(|e| e.to_string()))
        .collect()
}

/// Parses every document of `text`, dropping unparsable and unrendered ones.
pub fn snapshots(text: &str) -> Vec<Snapshot> {
    split_docs(text)
        .into_iter()
        .filter_map(|d| parse_doc(&d.ok()?).ok().flatten())
        .collect()
}

/// Round-robin across sources: first of each, then second of each, in listed order.
pub fn interleave<T>(sources: Vec<Vec<T>>) -> Vec<T> {
    let mut iters: Vec<_> = sources.into_iter().map(Vec::into_iter).collect();
    let mut out = Vec::new();
    loop {
        let before = out.len();
        for it in &mut iters {
            out.extend(it.next());
        }
        if out.len() == before {
            return out;
        }
    }
}

/// Replays fixture files, sleeping `replay` between documents.
pub fn spawn_fixture(paths: Vec<PathBuf>, replay: Duration, tx: Sender<Msg>) {
    thread::spawn(move || {
        let per_file = paths
            .iter()
            .map(|p| snapshots(&std::fs::read_to_string(p).unwrap_or_default()))
            .collect();
        for (i, snap) in interleave(per_file).into_iter().enumerate() {
            if i > 0 {
                thread::sleep(replay);
            }
            if tx.send(Msg::Doc(snap)).is_err() {
                return;
            }
        }
        let _ = tx.send(Msg::SourceEnded);
    });
}

/// Owns the `crew dash` children and reaps them on drop.
pub struct LiveSource {
    children: Vec<Child>,
}

impl Drop for LiveSource {
    fn drop(&mut self) {
        for c in &mut self.children {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

pub fn spawn_live(views: &[&str], tx: &Sender<Msg>) -> std::io::Result<LiveSource> {
    let mut live = LiveSource {
        children: Vec::new(),
    };
    for view in views {
        let mut child = Command::new("crew")
            .args(["dash", "--json", "--view", view, "--watch"])
            .stdout(Stdio::piped())
            .spawn()?;
        let stdout = child.stdout.take().expect("stdout is piped");
        live.children.push(child);
        let tx = tx.clone();
        thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                for snap in snapshots(&line) {
                    if tx.send(Msg::Doc(snap)).is_err() {
                        return;
                    }
                }
            }
            let _ = tx.send(Msg::SourceEnded);
        });
    }
    Ok(live)
}

pub fn spawn_input(tx: Sender<Msg>) {
    thread::spawn(move || {
        while let Ok(ev) = event::read() {
            let msg = match ev {
                Event::Key(k) if k.kind == KeyEventKind::Press => Msg::Key(k),
                Event::Resize(..) => Msg::Resize,
                _ => continue,
            };
            if tx.send(msg).is_err() {
                return;
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    const TWO_DOCS: &str = concat!(
        "{\"view\":\"map\",\"workers\":[]}\n",
        "{\"view\":\"overview\",\"attention\":[],\"agents\":[]}\n",
        "{\"view\":\"roster\"}\n",
    );

    #[test]
    fn splits_one_json_value_per_line() {
        assert_eq!(split_docs(TWO_DOCS).len(), 3);
    }

    #[test]
    fn two_documents_yield_two_snapshots() {
        let got = snapshots(TWO_DOCS);
        assert_eq!(got.len(), 2);
        assert!(matches!(got[0], Snapshot::Map { .. }));
        assert!(matches!(got[1], Snapshot::Overview { .. }));
    }

    #[test]
    fn pretty_printed_document_is_one_value() {
        assert_eq!(
            snapshots("{\n \"view\": \"map\",\n \"workers\": []\n}\n").len(),
            1
        );
    }

    #[test]
    fn interleaves_in_listed_order() {
        let got = interleave(vec![vec![1, 2, 3], vec![10, 20]]);
        assert_eq!(got, vec![1, 10, 2, 20, 3]);
    }
}
