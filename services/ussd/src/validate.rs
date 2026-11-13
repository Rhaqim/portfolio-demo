//! Checks a visitor-edited menu before the framework runs it.
//!
//! The demo lets anyone edit the menu JSON, so three things must hold before
//! `UssdApp::run` sees it: it is a sane size, no chain of non-interactive
//! screens (Initial / Function / Router) loops back on itself, which would
//! spin the request loop forever, and no service can call out over HTTP.
//! Webhook functions are a real framework feature; on a public demo they
//! would be an open proxy, so `function_url` is stripped.

use std::collections::{HashMap, HashSet};

use serde::Serialize;
use serde_json::Value;

const MAX_SCREENS: usize = 150;
const MAX_SERVICES: usize = 50;
const MAX_REGEX: usize = 200;
/// Most networks cut a USSD page at 182 characters.
const USSD_PAGE: usize = 182;
const KINDS: &[&str] = &["Initial", "Menu", "Input", "Function", "Router", "Quit"];

#[derive(Serialize, Default, Debug)]
pub struct Report {
    pub errors: Vec<String>,
    pub warnings: Vec<String>,
}

impl Report {
    pub fn ok(&self) -> bool {
        self.errors.is_empty()
    }
}

/// Validates `menu` in place, removing webhook URLs, and reports problems.
pub fn check(menu: &mut Value, registered: &HashSet<&str>) -> Report {
    let mut r = Report::default();

    let Some(root) = menu.as_object_mut() else {
        r.errors
            .push("The menu must be a JSON object with \"menus\" and \"services\".".into());
        return r;
    };

    // Services: strip webhooks, check the function exists.
    let mut service_names: HashSet<String> = HashSet::new();
    match root.get_mut("services") {
        Some(Value::Object(services)) => {
            if services.len() > MAX_SERVICES {
                r.errors.push(format!("At most {MAX_SERVICES} services."));
            }
            for (name, svc) in services.iter_mut() {
                service_names.insert(name.clone());
                if let Some(obj) = svc.as_object_mut() {
                    if obj.remove("function_url").is_some() {
                        r.warnings.push(format!(
                            "Service \"{name}\": function_url removed. Webhook functions are switched off in this demo."
                        ));
                    }
                    match obj.get("function_name").and_then(Value::as_str) {
                        Some(f) if registered.contains(f) => {}
                        Some(f) => r.warnings.push(format!(
                            "Service \"{name}\" calls \"{f}\", which isn't registered here. It will return an error value."
                        )),
                        None => r.errors.push(format!("Service \"{name}\" needs a function_name.")),
                    }
                    if obj.get("data_key").and_then(Value::as_str).is_none() {
                        r.errors
                            .push(format!("Service \"{name}\" needs a data_key."));
                    }
                }
            }
        }
        None => {
            root.insert("services".into(), Value::Object(Default::default()));
        }
        _ => r.errors.push("\"services\" must be an object.".into()),
    }

    let Some(Value::Object(screens)) = root.get("menus") else {
        r.errors
            .push("\"menus\" must be an object of screens.".into());
        return r;
    };
    if screens.is_empty() {
        r.errors.push("Add at least one screen.".into());
        return r;
    }
    if screens.len() > MAX_SCREENS {
        r.errors.push(format!("At most {MAX_SCREENS} screens."));
        return r;
    }

    let mut kind: HashMap<&str, &str> = HashMap::new();
    let mut edges: HashMap<&str, Vec<&str>> = HashMap::new();
    let mut initials = 0;

    for (name, s) in screens {
        let Some(obj) = s.as_object() else {
            r.errors
                .push(format!("Screen \"{name}\" must be an object."));
            continue;
        };
        let k = obj.get("screen_type").and_then(Value::as_str).unwrap_or("");
        if !KINDS.contains(&k) {
            r.errors.push(format!(
                "Screen \"{name}\": screen_type must be one of {}.",
                KINDS.join(", ")
            ));
            continue;
        }
        if obj.get("text").is_none() {
            r.errors.push(format!(
                "Screen \"{name}\" needs a \"text\" field (it can be empty)."
            ));
        }
        let next = obj.get("default_next_screen").and_then(Value::as_str);
        if next.is_none() {
            r.errors
                .push(format!("Screen \"{name}\" needs a default_next_screen."));
        }
        if k == "Initial" {
            initials += 1;
        }
        kind.insert(name, k);

        let mut out: Vec<&str> = next.into_iter().collect();
        if let Some(Value::Array(opts)) = obj.get("router_options") {
            for o in opts {
                if let Some(n) = o.get("next_screen").and_then(Value::as_str) {
                    out.push(n);
                }
            }
        }
        if let Some(Value::Object(items)) = obj.get("menu_items") {
            for it in items.values() {
                if let Some(n) = it.get("next_screen").and_then(Value::as_str) {
                    out.push(n);
                }
            }
        }
        if let Some(t) = obj.get("timeout_screen").and_then(Value::as_str) {
            out.push(t);
        }
        edges.insert(name, out);

        if k == "Function" {
            match obj.get("function").and_then(Value::as_str) {
                Some(f) if service_names.contains(f) => {}
                Some(f) => r.errors.push(format!(
                    "Screen \"{name}\" calls service \"{f}\", which isn't in \"services\"."
                )),
                None => r
                    .errors
                    .push(format!("Function screen \"{name}\" needs a \"function\".")),
            }
        }
        if k == "Menu"
            && obj
                .get("menu_items")
                .and_then(Value::as_object)
                .map_or(true, |m| m.is_empty())
        {
            r.warnings
                .push(format!("Menu \"{name}\" has no menu_items."));
        }
        if let Some(re) = obj.get("validation_regex").and_then(Value::as_str) {
            if re.len() > MAX_REGEX {
                r.errors.push(format!(
                    "Screen \"{name}\": validation_regex is limited to {MAX_REGEX} characters."
                ));
            }
        }
        if matches!(k, "Menu" | "Input" | "Quit") {
            if let Some(len) = longest_text(obj.get("text")) {
                if len > USSD_PAGE {
                    r.warnings.push(format!(
                        "Screen \"{name}\" is {len} characters; most networks cut a USSD page at {USSD_PAGE}."
                    ));
                }
            }
        }
    }

    match initials {
        0 => r.errors.push(
            "One screen must have screen_type \"Initial\"; it's where every session starts.".into(),
        ),
        1 => {}
        _ => r.errors.push("Only one screen can be \"Initial\".".into()),
    }

    for (from, tos) in &edges {
        for to in tos {
            if !kind.contains_key(to) {
                r.warnings.push(format!(
                    "\"{from}\" points to \"{to}\", which doesn't exist."
                ));
            }
        }
    }

    if let Some(cycle) = passthrough_cycle(&kind, &edges) {
        r.errors.push(format!(
            "These screens loop without ever waiting for input: {}. Put a Menu, Input or Quit screen in the loop.",
            cycle.join(" → ")
        ));
    }

    r
}

fn longest_text(v: Option<&Value>) -> Option<usize> {
    match v? {
        Value::String(s) => Some(s.chars().count()),
        Value::Object(m) => m
            .values()
            .filter_map(Value::as_str)
            .map(|s| s.chars().count())
            .max(),
        _ => None,
    }
}

/// Finds a cycle made only of screens the framework passes straight through.
fn passthrough_cycle<'a>(
    kind: &HashMap<&'a str, &'a str>,
    edges: &HashMap<&'a str, Vec<&'a str>>,
) -> Option<Vec<&'a str>> {
    let passes = |n: &str| {
        matches!(
            kind.get(n),
            Some(&"Initial") | Some(&"Function") | Some(&"Router")
        )
    };
    #[derive(Clone, Copy, PartialEq)]
    enum Mark {
        New,
        Open,
        Done,
    }
    let mut mark: HashMap<&str, Mark> = kind.keys().map(|k| (*k, Mark::New)).collect();
    let mut path: Vec<&str> = Vec::new();

    fn visit<'a>(
        n: &'a str,
        edges: &HashMap<&'a str, Vec<&'a str>>,
        passes: &dyn Fn(&str) -> bool,
        mark: &mut HashMap<&'a str, Mark>,
        path: &mut Vec<&'a str>,
    ) -> Option<Vec<&'a str>> {
        mark.insert(n, Mark::Open);
        path.push(n);
        for &m in edges.get(n).map(Vec::as_slice).unwrap_or(&[]) {
            if !passes(m) {
                continue;
            }
            match mark.get(m).copied() {
                Some(Mark::Open) => {
                    let start = path.iter().position(|p| *p == m).unwrap_or(0);
                    let mut c = path[start..].to_vec();
                    c.push(m);
                    return Some(c);
                }
                Some(Mark::New) => {
                    if let Some(c) = visit(m, edges, passes, mark, path) {
                        return Some(c);
                    }
                }
                _ => {}
            }
        }
        path.pop();
        mark.insert(n, Mark::Done);
        None
    }

    let starts: Vec<&str> = kind.keys().copied().filter(|n| passes(n)).collect();
    for n in starts {
        if mark.get(n) == Some(&Mark::New) {
            if let Some(c) = visit(n, edges, &passes, &mut mark, &mut path) {
                return Some(c);
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn reg() -> HashSet<&'static str> {
        ["check_limit"].into_iter().collect()
    }

    #[test]
    fn default_menu_is_valid() {
        let mut m: Value = serde_json::from_str(include_str!("../menu.json")).unwrap();
        let registered: HashSet<&str> = crate::functions::CATALOGUE
            .iter()
            .map(|(n, _)| *n)
            .collect();
        let r = check(&mut m, &registered);
        assert!(r.ok(), "{:?}", r.errors);
        assert!(r.warnings.is_empty(), "{:?}", r.warnings);
    }

    #[test]
    fn detects_passthrough_loop() {
        let mut m = json!({
            "menus": {
                "Start": {"text": "", "screen_type": "Initial", "default_next_screen": "R"},
                "R": {"text": "", "screen_type": "Router", "default_next_screen": "F", "router_options": []},
                "F": {"text": "", "screen_type": "Function", "function": "s", "default_next_screen": "R"}
            },
            "services": {"s": {"function_name": "check_limit", "data_key": "x"}}
        });
        let r = check(&mut m, &reg());
        assert!(
            r.errors.iter().any(|e| e.contains("loop")),
            "{:?}",
            r.errors
        );
    }

    #[test]
    fn loop_through_a_menu_is_fine() {
        let mut m = json!({
            "menus": {
                "Start": {"text": "", "screen_type": "Initial", "default_next_screen": "M"},
                "M": {"text": "Hi", "screen_type": "Menu", "default_next_screen": "M",
                      "menu_items": {"a": {"option": "1", "display_name": "Again", "next_screen": "Start"}}}
            },
            "services": {}
        });
        assert!(check(&mut m, &reg()).ok());
    }

    #[test]
    fn strips_webhooks() {
        let mut m = json!({
            "menus": {"Start": {"text": "", "screen_type": "Initial", "default_next_screen": "Q"},
                      "Q": {"text": "Bye", "screen_type": "Quit", "default_next_screen": "Start"}},
            "services": {"s": {"function_name": "nope", "function_url": "http://169.254.169.254/", "data_key": "x"}}
        });
        let r = check(&mut m, &reg());
        assert!(m["services"]["s"].get("function_url").is_none());
        assert!(r.warnings.iter().any(|w| w.contains("function_url")));
    }
}
