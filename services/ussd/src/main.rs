//! HTTP front for the ussdframework demo.
//!
//! The browser draws a phone and posts each keypress here. Every request runs
//! through the real framework (`UssdApp::run`) against either the default
//! QuickLoan menu or a menu the visitor edited, and the response carries the
//! session state so the page can show what the framework is doing.

mod functions;
mod validate;

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use actix_web::{
    get, middleware, post, web, App, HttpRequest, HttpResponse, HttpServer, Responder,
};
use serde::Deserialize;
use serde_json::{json, Value};
use ussdframework::prelude::*;

const DEFAULT_MENU: &str = include_str!("../menu.json");
const SESSION_TTL: Duration = Duration::from_secs(10 * 60);
const MAX_SESSIONS: usize = 50_000;

// ---------------------------------------------------------------------------
// Session store: the framework's SessionCache trait, backed by a TTL map.
// A production deployment would put Redis here; the trait is the seam.
// ---------------------------------------------------------------------------

struct TtlStore {
    inner: Mutex<HashMap<String, (Instant, String)>>,
}

impl SessionCache for TtlStore {
    fn store_session(&self, s: &USSDSession) -> Result<(), String> {
        let json = serde_json::to_string(s).map_err(|e| e.to_string())?;
        let mut m = self.inner.lock().map_err(|e| e.to_string())?;
        if m.len() >= MAX_SESSIONS {
            m.retain(|_, (t, _)| t.elapsed() < SESSION_TTL);
            if m.len() >= MAX_SESSIONS {
                m.clear();
            }
        }
        m.insert(s.session_id.clone(), (Instant::now(), json));
        Ok(())
    }

    fn retrieve_session(&self, id: &str) -> Result<Option<USSDSession>, String> {
        let m = self.inner.lock().map_err(|e| e.to_string())?;
        match m.get(id) {
            Some((t, json)) if t.elapsed() < SESSION_TTL => serde_json::from_str(json)
                .map(Some)
                .map_err(|e| e.to_string()),
            _ => Ok(None),
        }
    }
}

// ---------------------------------------------------------------------------
// Rate limiting: fixed window per client IP.
// ---------------------------------------------------------------------------

struct Limiter {
    hits: Mutex<HashMap<String, (Instant, u32)>>,
    limit: u32,
    window: Duration,
}

impl Limiter {
    fn allow(&self, key: &str) -> bool {
        let mut m = self.hits.lock().unwrap_or_else(|e| e.into_inner());
        if m.len() > 100_000 {
            m.retain(|_, (t, _)| t.elapsed() < self.window);
        }
        let e = m.entry(key.to_string()).or_insert((Instant::now(), 0));
        if e.0.elapsed() >= self.window {
            *e = (Instant::now(), 0);
        }
        e.1 += 1;
        e.1 <= self.limit
    }
}

fn client_ip(req: &HttpRequest) -> String {
    // Behind Cloudflare and Caddy; see the README on locking the origin to Cloudflare.
    if let Some(v) = req
        .headers()
        .get("CF-Connecting-IP")
        .and_then(|v| v.to_str().ok())
    {
        return v.to_string();
    }
    req.connection_info()
        .realip_remote_addr()
        .unwrap_or("unknown")
        .to_string()
}

struct State {
    app: UssdApp,
    default_menu: USSDMenu,
    registered: HashSet<&'static str>,
    limiter: Limiter,
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
struct DialRequest {
    session_id: String,
    msisdn: String,
    #[serde(default)]
    input: String,
    /// A visitor-edited menu. Absent means the default QuickLoan menu.
    #[serde(default)]
    menu: Option<Value>,
}

fn bad(msg: impl Into<String>) -> HttpResponse {
    HttpResponse::BadRequest().json(json!({ "error": msg.into() }))
}

#[post("/api/ussd/dial")]
async fn dial(
    req: HttpRequest,
    state: web::Data<State>,
    body: web::Json<DialRequest>,
) -> impl Responder {
    if !state.limiter.allow(&client_ip(&req)) {
        return HttpResponse::TooManyRequests()
            .json(json!({ "error": "Slow down a little, then dial again." }));
    }
    let b = body.into_inner();
    if b.session_id.is_empty()
        || b.session_id.len() > 64
        || !b
            .session_id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-')
    {
        return bad("invalid session_id");
    }
    if b.msisdn.is_empty() || b.msisdn.len() > 15 || !b.msisdn.chars().all(|c| c.is_ascii_digit()) {
        return bad("invalid msisdn");
    }
    // USSD carries at most 182 characters per message.
    let input: String = b.input.chars().take(182).collect();

    let (menu, report) = match b.menu {
        None => (state.default_menu.clone(), validate::Report::default()),
        Some(mut v) => {
            let report = validate::check(&mut v, &state.registered);
            if !report.ok() {
                return HttpResponse::UnprocessableEntity()
                    .json(json!({ "error": "The menu has errors.", "report": report }));
            }
            match serde_json::from_value::<USSDMenu>(v) {
                Ok(m) => (m, report),
                Err(e) => {
                    return HttpResponse::UnprocessableEntity().json(json!({
                        "error": "The menu doesn't match the framework's schema.",
                        "report": { "errors": [e.to_string()], "warnings": [] }
                    }))
                }
            }
        }
    };

    let request = USSDRequest {
        msisdn: b.msisdn,
        input,
        session_id: b.session_id.clone(),
        service_code: "*384#".into(),
        language: "en".into(),
    };
    let started = Instant::now();
    let response = state.app.run(request, menu);
    let elapsed_us = started.elapsed().as_micros() as u64;

    let session = state
        .app
        .session_cache
        .retrieve_session(&b.session_id)
        .ok()
        .flatten();
    HttpResponse::Ok().json(json!({
        "message": response.message,
        "end_session": response.end_session,
        "gateway": response.to_gateway_string(),
        "elapsed_us": elapsed_us,
        "session": session.map(|s| json!({
            "current_screen": s.current_screen,
            "visited_screens": s.visited_screens,
            "data": s.data.iter().map(|(k, v)| (k.clone(), plain(v))).collect::<serde_json::Map<_, _>>(),
            "attempts": s.screen_attempts,
        })),
        "warnings": report.warnings,
    }))
}

/// USSDData serialises externally tagged ({"Str": "x"}); flatten it for display.
fn plain(d: &USSDData) -> Value {
    match d {
        USSDData::Str(s) => Value::String(s.clone()),
        USSDData::Int(i) => json!(i),
        USSDData::Float(f) => json!(f),
        USSDData::List(l) => Value::Array(l.iter().map(plain).collect()),
        USSDData::ListStr(l) => json!(l),
        USSDData::Dict(m) => Value::Object(m.iter().map(|(k, v)| (k.clone(), plain(v))).collect()),
        USSDData::None => Value::Null,
    }
}

#[post("/api/ussd/validate")]
async fn validate_menu(state: web::Data<State>, body: web::Json<Value>) -> impl Responder {
    let mut v = body.into_inner();
    let mut report = validate::check(&mut v, &state.registered);
    if report.ok() {
        if let Err(e) = serde_json::from_value::<USSDMenu>(v) {
            report.errors.push(format!("Schema: {e}"));
        }
    }
    HttpResponse::Ok().json(report)
}

#[get("/api/ussd/menu")]
async fn get_menu() -> impl Responder {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(DEFAULT_MENU)
}

#[get("/api/ussd/functions")]
async fn list_functions() -> impl Responder {
    let list: Vec<Value> = functions::CATALOGUE
        .iter()
        .map(|(n, d)| json!({ "name": n, "description": d }))
        .collect();
    HttpResponse::Ok().json(list)
}

/// The same app behind the Africa's Talking gateway format, to show the
/// framework speaks a real telco protocol: form fields in, "CON …"/"END …" out.
#[post("/api/ussd/africastalking")]
async fn africas_talking(
    req: HttpRequest,
    state: web::Data<State>,
    form: web::Form<AfricasTalkingRequest>,
) -> impl Responder {
    if !state.limiter.allow(&client_ip(&req)) {
        return HttpResponse::TooManyRequests().body("END Too many requests.");
    }
    let r = form.into_inner().into_ussd_request();
    if r.session_id.len() > 64 || r.msisdn.len() > 20 {
        return HttpResponse::BadRequest().body("END Invalid request.");
    }
    let resp = state.app.run(r, state.default_menu.clone());
    HttpResponse::Ok()
        .content_type("text/plain; charset=utf-8")
        .body(resp.to_gateway_string())
}

#[get("/api/ussd/health")]
async fn health() -> impl Responder {
    HttpResponse::Ok().json(json!({ "status": "ok" }))
}

#[actix_web::main]
async fn main() -> std::io::Result<()> {
    let default_menu: USSDMenu = serde_json::from_str(DEFAULT_MENU).expect("menu.json must parse");
    let mut app = UssdApp::new(
        false,
        Some(Box::new(TtlStore {
            inner: Mutex::new(HashMap::new()),
        })),
    );
    app.register_functions(functions::registry());

    let state = web::Data::new(State {
        app,
        default_menu,
        registered: functions::CATALOGUE.iter().map(|(n, _)| *n).collect(),
        limiter: Limiter {
            hits: Mutex::new(HashMap::new()),
            limit: 600,
            window: Duration::from_secs(600),
        },
    });

    let addr = std::env::var("ADDR").unwrap_or_else(|_| "0.0.0.0:8080".into());
    // ussdframework logs through the `log` facade; no logger is installed, so
    // the framework stays silent and never prints session data.
    eprintln!("{{\"service\":\"ussd\",\"msg\":\"listening\",\"addr\":\"{addr}\"}}");
    HttpServer::new(move || {
        App::new()
            .app_data(state.clone())
            .app_data(web::JsonConfig::default().limit(96 * 1024))
            .wrap(middleware::DefaultHeaders::new().add(("Cache-Control", "no-store")))
            .service(dial)
            .service(validate_menu)
            .service(get_menu)
            .service(list_functions)
            .service(africas_talking)
            .service(health)
    })
    .workers(2)
    .bind(addr)?
    .run()
    .await
}
