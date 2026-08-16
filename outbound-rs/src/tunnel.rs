use std::collections::HashMap;

pub const HEADER_AGENT: &str = "X-Outbound-Agent";
pub const HEADER_SERVICE: &str = "X-Outbound-Service";
pub const HEADER_AUTH: &str = "X-Outbound-Authorization";

const HEX_DIGITS: &[u8; 16] = b"0123456789abcdef";

const HOP_BY_HOP_HEADERS: [&str; 7] = [
    "connection",
    "keep-alive",
    "proxy-connection",
    "transfer-encoding",
    "upgrade",
    "te",
    "trailer",
];

pub fn is_hop_by_hop_header(name: &str) -> bool {
    HOP_BY_HOP_HEADERS
        .iter()
        .any(|header| name.eq_ignore_ascii_case(header))
}

pub fn new_request_id() -> Result<String, getrandom::Error> {
    let mut bytes = [0u8; 16];
    getrandom::fill(&mut bytes)?;

    let mut request_id = String::with_capacity(32);

    for byte in bytes {
        request_id.push(HEX_DIGITS[(byte >> 4) as usize] as char);
        request_id.push(HEX_DIGITS[(byte & 0x0f) as usize] as char);
    }

    Ok(request_id)
}

pub fn headers_to_map(
    headers: &HashMap<String, Vec<String>>,
    drop_keys: &[&str],
) -> HashMap<String, String> {
    let mut output = HashMap::with_capacity(headers.len());

    for (name, values) in headers {
        let must_drop = is_hop_by_hop_header(name)
            || drop_keys
                .iter()
                .any(|drop_key| name.eq_ignore_ascii_case(drop_key));

        if must_drop {
            continue;
        }

        output.insert(name.clone(), values.join(","));
    }

    output
}

pub fn map_to_headers(
    destination: &mut HashMap<String, Vec<String>>,
    source: &HashMap<String, String>,
) {
    for (name, value) in source {
        if is_hop_by_hop_header(name) {
            continue;
        }

        destination.insert(name.clone(), vec![value.clone()]);
    }
}
