use serde_json::Value;

/// Worker identity: `(crew, branch)`.
pub type Key = (String, String);

#[derive(Debug, Clone, PartialEq)]
pub struct WorkerView {
    pub key: Key,
    pub state: String,
    pub name: Option<String>,
    pub detail: Option<String>,
}

impl WorkerView {
    /// Most names are null in practice; the branch is the stable fallback.
    pub fn label(&self) -> &str {
        self.name.as_deref().unwrap_or(&self.key.1)
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct RoleView {
    pub lead_key: Key,
    pub role: String,
    pub state: Option<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct AttentionItem {
    pub id: String,
    pub severity: String,
    pub kind: String,
    pub message: String,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Snapshot {
    Map {
        workers: Vec<WorkerView>,
    },
    Overview {
        attention: Vec<AttentionItem>,
        roles: Vec<RoleView>,
    },
}

fn str_field(v: &Value, field: &str) -> Option<String> {
    v.get(field)?.as_str().map(str::to_owned)
}

fn req_str(v: &Value, field: &str) -> Result<String, String> {
    str_field(v, field).ok_or_else(|| format!("missing string field `{field}`"))
}

fn array<'a>(v: &'a Value, field: &str) -> Result<&'a Vec<Value>, String> {
    v.get(field)
        .and_then(Value::as_array)
        .ok_or_else(|| format!("missing array field `{field}`"))
}

pub fn parse_map(doc: &Value) -> Result<Snapshot, String> {
    let workers = array(doc, "workers")?
        .iter()
        .map(|w| {
            Ok(WorkerView {
                key: (req_str(w, "crew")?, req_str(w, "branch")?),
                state: req_str(w, "state")?,
                name: str_field(w, "name"),
                detail: str_field(w, "detail"),
            })
        })
        .collect::<Result<_, String>>()?;
    Ok(Snapshot::Map { workers })
}

pub fn parse_overview(doc: &Value) -> Result<Snapshot, String> {
    let attention = array(doc, "attention")?
        .iter()
        .map(|a| {
            Ok(AttentionItem {
                id: req_str(a, "id")?,
                severity: req_str(a, "severity")?,
                kind: req_str(a, "kind")?,
                message: req_str(a, "message")?,
            })
        })
        .collect::<Result<_, String>>()?;
    let mut roles = Vec::new();
    for agent in array(doc, "agents")? {
        let lead_key = (req_str(agent, "crew")?, req_str(agent, "branch")?);
        for r in agent
            .get("roles")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            roles.push(RoleView {
                lead_key: lead_key.clone(),
                role: req_str(r, "role")?,
                state: str_field(r, "state"),
            });
        }
    }
    Ok(Snapshot::Overview { attention, roles })
}

/// Dispatches on the `view` field; views this spike does not render yield `Ok(None)`.
pub fn parse_doc(doc: &Value) -> Result<Option<Snapshot>, String> {
    match doc.get("view").and_then(Value::as_str) {
        Some("map") => parse_map(doc).map(Some),
        Some("overview") => parse_overview(doc).map(Some),
        _ => Ok(None),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn golden(s: &str) -> Value {
        serde_json::from_str(s).expect("golden is one JSON object")
    }

    #[test]
    fn map_golden() {
        let doc = golden(include_str!(
            "../../../dash/internal/view/testdata/golden/map.json"
        ));
        let Some(Snapshot::Map { workers }) = parse_doc(&doc).unwrap() else {
            panic!("expected a map snapshot");
        };
        assert_eq!(workers.len(), 11);
        let otter = workers
            .iter()
            .find(|w| w.name.as_deref() == Some("otter"))
            .expect("otter exists");
        assert_eq!(otter.state, "working");
        assert_eq!(otter.label(), "otter");
        let nameless = workers.iter().find(|w| w.name.is_none()).unwrap();
        assert_eq!(nameless.label(), nameless.key.1);
    }

    #[test]
    fn overview_golden() {
        let doc = golden(include_str!(
            "../../../dash/internal/view/testdata/golden/overview.json"
        ));
        let Some(Snapshot::Overview { attention, roles }) = parse_doc(&doc).unwrap() else {
            panic!("expected an overview snapshot");
        };
        assert_eq!(attention.len(), 12);
        assert!(attention.iter().any(|a| a.id == "failed:c1:feat/h"));
        assert!(roles
            .iter()
            .any(|r| r.role == "reviewer" && r.state.as_deref() == Some("blocked")));
    }

    #[test]
    fn unknown_view_is_ignored() {
        let doc = serde_json::json!({"view": "roster"});
        assert_eq!(parse_doc(&doc), Ok(None));
    }
}
