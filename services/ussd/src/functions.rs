//! The business logic behind the demo menu: a fictional mobile-loan product.
//!
//! These are ordinary `ussdframework` functions (`fn(&USSDSession, &str) ->
//! USSDData`) registered by name and called from `Function` screens. State
//! lives in a small in-memory ledger keyed by the caller's phone number, so a
//! loan you take shows up when you repay or check your statement.

use std::collections::HashMap;
use std::sync::{Mutex, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

use chrono::{Duration, Utc};
use ussdframework::prelude::{FunctionMap, USSDData, USSDFunction, USSDSession};

/// Name and one-line description of every registered function, for the UI.
pub const CATALOGUE: &[(&str, &str)] = &[
    ("check_limit", "Loan limit and what's available now → limit, available, status"),
    ("quote_loan", "Prices `amount` over `days` → amount, fee, total, due, available, status (ok | over_limit)"),
    ("disburse_loan", "Checks `pin` (0000 is wrong) and pays out the quote → amount, total, due, reference, status (ok | bad_pin)"),
    ("repay_info", "Outstanding balance → total, due, status (due | none)"),
    ("repay_loan", "Checks `pin` and settles the balance → amount, status (ok | bad_pin | none)"),
    ("mini_statement", "Last three transactions → lines"),
];

pub fn registry() -> FunctionMap {
    let mut m: FunctionMap = HashMap::new();
    m.insert("check_limit".into(), check_limit as USSDFunction);
    m.insert("quote_loan".into(), quote_loan as USSDFunction);
    m.insert("disburse_loan".into(), disburse_loan as USSDFunction);
    m.insert("repay_info".into(), repay_info as USSDFunction);
    m.insert("repay_loan".into(), repay_loan as USSDFunction);
    m.insert("mini_statement".into(), mini_statement as USSDFunction);
    m
}

#[derive(Default, Clone)]
struct Account {
    outstanding: u64,
    due: String,
    history: Vec<String>,
}

fn ledger() -> &'static Mutex<HashMap<String, Account>> {
    static L: OnceLock<Mutex<HashMap<String, Account>>> = OnceLock::new();
    L.get_or_init(|| Mutex::new(HashMap::new()))
}

fn with_account<T>(msisdn: &str, f: impl FnOnce(&mut Account) -> T) -> T {
    let mut l = ledger().lock().unwrap_or_else(|e| e.into_inner());
    // A demo, not a bank: cap memory by forgetting everyone at once.
    if l.len() > 20_000 {
        l.clear();
    }
    f(l.entry(msisdn.to_string()).or_default())
}

/// A stable, phone-number-derived limit between KES 500 and 5,000.
fn limit_for(msisdn: &str) -> u64 {
    let h = msisdn.bytes().fold(1469598103934665603u64, |h, b| {
        (h ^ b as u64).wrapping_mul(1099511628211)
    });
    500 + (h % 46) * 100
}

fn fee_rate(days: u64) -> f64 {
    match days {
        7 => 0.07,
        14 => 0.10,
        _ => 0.15,
    }
}

fn data_str(session: &USSDSession, key: &str) -> String {
    match session.data.get(key) {
        Some(USSDData::Str(s)) => s.clone(),
        Some(USSDData::Int(i)) => i.to_string(),
        Some(USSDData::Dict(d)) => match d.get("amount") {
            Some(USSDData::Str(s)) => s.clone(),
            _ => String::new(),
        },
        _ => String::new(),
    }
}

fn dict(pairs: &[(&str, String)]) -> USSDData {
    USSDData::Dict(
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), USSDData::Str(v.clone())))
            .collect(),
    )
}

/// 12345 -> "12,345"
pub fn money(n: u64) -> String {
    let s = n.to_string();
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if i > 0 && (s.len() - i) % 3 == 0 {
            out.push(',');
        }
        out.push(c);
    }
    out
}

fn reference() -> String {
    let n = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let alphabet = b"ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    let mut x = n as u64 ^ 0x9E3779B97F4A7C15;
    let mut s = String::from("QL");
    for _ in 0..8 {
        s.push(alphabet[(x % alphabet.len() as u64) as usize] as char);
        x /= alphabet.len() as u64;
        x = x
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
    }
    s
}

fn available(msisdn: &str) -> u64 {
    let limit = limit_for(msisdn);
    with_account(msisdn, |a| limit.saturating_sub(a.outstanding))
}

fn check_limit(session: &USSDSession, _: &str) -> USSDData {
    let limit = limit_for(&session.msisdn);
    dict(&[
        ("status", "ok".into()),
        ("limit", money(limit)),
        ("available", money(available(&session.msisdn))),
    ])
}

fn quote_loan(session: &USSDSession, _: &str) -> USSDData {
    let amount: u64 = data_str(session, "amount").parse().unwrap_or(0);
    let days: u64 = data_str(session, "days").parse().unwrap_or(30);
    let avail = available(&session.msisdn);
    let fee = (amount as f64 * fee_rate(days)).round() as u64;
    let due = (Utc::now() + Duration::days(days as i64))
        .format("%-d %b")
        .to_string();
    let status = if amount == 0 {
        "error"
    } else if amount > avail {
        "over_limit"
    } else {
        "ok"
    };
    dict(&[
        ("status", status.into()),
        ("amount", money(amount)),
        ("fee", money(fee)),
        ("total", money(amount + fee)),
        ("due", due),
        ("available", money(avail)),
    ])
}

fn disburse_loan(session: &USSDSession, _: &str) -> USSDData {
    if data_str(session, "pin") == "0000" {
        return dict(&[("status", "bad_pin".into())]);
    }
    let amount: u64 = data_str(session, "amount").parse().unwrap_or(0);
    let days: u64 = data_str(session, "days").parse().unwrap_or(30);
    if amount == 0 || amount > available(&session.msisdn) {
        return dict(&[("status", "error".into())]);
    }
    let total = amount + (amount as f64 * fee_rate(days)).round() as u64;
    let due = (Utc::now() + Duration::days(days as i64))
        .format("%-d %b")
        .to_string();
    let r = reference();
    with_account(&session.msisdn, |a| {
        a.outstanding += total;
        a.due = due.clone();
        a.history.push(format!(
            "{} Loan +{}",
            Utc::now().format("%-d %b"),
            money(amount)
        ));
    });
    dict(&[
        ("status", "ok".into()),
        ("amount", money(amount)),
        ("total", money(total)),
        ("due", due),
        ("reference", r),
    ])
}

fn repay_info(session: &USSDSession, _: &str) -> USSDData {
    let (owed, due) = with_account(&session.msisdn, |a| (a.outstanding, a.due.clone()));
    if owed == 0 {
        return dict(&[("status", "none".into())]);
    }
    dict(&[
        ("status", "due".into()),
        ("total", money(owed)),
        ("due", due),
    ])
}

fn repay_loan(session: &USSDSession, _: &str) -> USSDData {
    if data_str(session, "pin") == "0000" {
        return dict(&[("status", "bad_pin".into())]);
    }
    let paid = with_account(&session.msisdn, |a| {
        let p = a.outstanding;
        if p > 0 {
            a.history.push(format!(
                "{} Repaid -{}",
                Utc::now().format("%-d %b"),
                money(p)
            ));
        }
        a.outstanding = 0;
        a.due.clear();
        p
    });
    if paid == 0 {
        return dict(&[("status", "none".into())]);
    }
    dict(&[("status", "ok".into()), ("amount", money(paid))])
}

fn mini_statement(session: &USSDSession, _: &str) -> USSDData {
    let lines = with_account(&session.msisdn, |a| {
        if a.history.is_empty() {
            "No transactions yet.".to_string()
        } else {
            a.history
                .iter()
                .rev()
                .take(3)
                .cloned()
                .collect::<Vec<_>>()
                .join("\n")
        }
    });
    dict(&[("lines", lines)])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn money_groups_thousands() {
        assert_eq!(money(0), "0");
        assert_eq!(money(999), "999");
        assert_eq!(money(1000), "1,000");
        assert_eq!(money(1234567), "1,234,567");
    }

    #[test]
    fn limit_is_in_range_and_stable() {
        for n in ["254700000001", "254711111111", "234800000000"] {
            let l = limit_for(n);
            assert!((500..=5000).contains(&l));
            assert_eq!(l, limit_for(n));
        }
    }
}
