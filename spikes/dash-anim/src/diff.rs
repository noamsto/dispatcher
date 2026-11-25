use crate::model::{AttentionItem, WorkerView};

#[derive(Debug, Clone, PartialEq)]
pub enum WorkerChange {
    Added(WorkerView),
    Changed {
        worker: WorkerView,
        from: String,
        to: String,
    },
    Removed(WorkerView),
}

#[derive(Debug, Clone, PartialEq)]
pub enum AttentionChange {
    Added(AttentionItem),
    Removed(AttentionItem),
}

pub fn diff_workers(old: &[WorkerView], new: &[WorkerView]) -> Vec<WorkerChange> {
    let mut out = Vec::new();
    for n in new {
        match old.iter().find(|o| o.key == n.key) {
            None => out.push(WorkerChange::Added(n.clone())),
            Some(o) if o.state != n.state => out.push(WorkerChange::Changed {
                worker: n.clone(),
                from: o.state.clone(),
                to: n.state.clone(),
            }),
            Some(_) => {}
        }
    }
    out.extend(
        old.iter()
            .filter(|o| !new.iter().any(|n| n.key == o.key))
            .map(|o| WorkerChange::Removed(o.clone())),
    );
    out
}

pub fn diff_attention(old: &[AttentionItem], new: &[AttentionItem]) -> Vec<AttentionChange> {
    let added = new
        .iter()
        .filter(|n| !old.iter().any(|o| o.id == n.id))
        .map(|n| AttentionChange::Added(n.clone()));
    let removed = old
        .iter()
        .filter(|o| !new.iter().any(|n| n.id == o.id))
        .map(|o| AttentionChange::Removed(o.clone()));
    added.chain(removed).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn w(branch: &str, state: &str, detail: &str) -> WorkerView {
        WorkerView {
            key: ("c1".into(), branch.into()),
            state: state.into(),
            name: None,
            detail: Some(detail.into()),
        }
    }

    fn a(id: &str) -> AttentionItem {
        AttentionItem {
            id: id.into(),
            severity: "critical".into(),
            kind: "failed".into(),
            message: "m".into(),
        }
    }

    #[test]
    fn worker_added() {
        let new = w("feat/a", "working", "x");
        let got = diff_workers(&[], std::slice::from_ref(&new));
        assert_eq!(got, vec![WorkerChange::Added(new)]);
    }

    #[test]
    fn worker_removed() {
        let old = w("feat/a", "working", "x");
        let got = diff_workers(std::slice::from_ref(&old), &[]);
        assert_eq!(got, vec![WorkerChange::Removed(old)]);
    }

    #[test]
    fn state_change() {
        let old = w("feat/a", "working", "x");
        let new = w("feat/a", "blocked", "x");
        let got = diff_workers(&[old], std::slice::from_ref(&new));
        assert_eq!(
            got,
            vec![WorkerChange::Changed {
                worker: new,
                from: "working".into(),
                to: "blocked".into(),
            }]
        );
    }

    #[test]
    fn detail_only_change_is_not_a_state_change() {
        let got = diff_workers(
            &[w("feat/a", "working", "x")],
            &[w("feat/a", "working", "y")],
        );
        assert!(got.is_empty());
    }

    #[test]
    fn unchanged_is_empty() {
        let s = [w("feat/a", "working", "x"), w("feat/b", "done", "y")];
        assert!(diff_workers(&s, &s).is_empty());
        assert!(diff_attention(&[a("x")], &[a("x")]).is_empty());
    }

    #[test]
    fn attention_added_and_removed_by_id() {
        let got = diff_attention(&[a("old")], &[a("new")]);
        assert_eq!(
            got,
            vec![
                AttentionChange::Added(a("new")),
                AttentionChange::Removed(a("old"))
            ]
        );
    }
}
