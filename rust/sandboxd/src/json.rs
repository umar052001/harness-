//! Minimal JSON emission for the executor's machine-facing output.
//!
//! Hand-rolled because the only dependency budget for the executor is Landlock;
//! pulling in serde to format two message shapes would be a poor trade. The Go
//! side decodes both with the generated Protobuf types, so a malformed field is a
//! decode error there rather than a silent mismatch here - which is what makes
//! skipping a JSON library acceptable.

/// Escape a string as a JSON string literal, quotes included.
///
/// Non-ASCII passes through as valid UTF-8 rather than being escaped: protojson
/// emits raw UTF-8 too, so the Go decoder is equally happy either way, and the
/// transcripts these carry are full of box-drawing and emoji from tool output.
pub fn string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            // Remaining control characters must be escaped; everything else is
            // already legal inside a JSON string.
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Render a list of strings as a JSON array.
pub fn string_array(items: &[String]) -> String {
    let mut out = String::from("[");
    for (i, s) in items.iter().enumerate() {
        if i > 0 {
            out.push_str(", ");
        }
        out.push_str(&string(s));
    }
    out.push(']');
    out
}

/// Standard base64 with padding, as protojson requires for `bytes` fields.
///
/// The pty carries arbitrary bytes, including partial UTF-8 sequences split
/// across reads, so streaming output has to be encoded as bytes rather than
/// embedded as a JSON string. Go refuses to decode anything that is not valid
/// base64, which makes this cheap to trust.
pub fn base64(input: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let b0 = chunk[0] as u32;
        let b1 = *chunk.get(1).unwrap_or(&0) as u32;
        let b2 = *chunk.get(2).unwrap_or(&0) as u32;
        let n = (b0 << 16) | (b1 << 8) | b2;

        out.push(ALPHABET[(n >> 18) as usize & 63] as char);
        out.push(ALPHABET[(n >> 12) as usize & 63] as char);
        // The final group is padded out to a multiple of four.
        out.push(if chunk.len() > 1 {
            ALPHABET[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            ALPHABET[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_matches_known_vectors() {
        // Vectors from RFC 4648 §10, which are the same ones Go's
        // encoding/base64.StdEncoding enforces on the other side.
        assert_eq!(base64(b""), "");
        assert_eq!(base64(b"f"), "Zg==");
        assert_eq!(base64(b"fo"), "Zm8=");
        assert_eq!(base64(b"foo"), "Zm9v");
        assert_eq!(base64(b"foob"), "Zm9vYg==");
        assert_eq!(base64(b"fooba"), "Zm9vYmE=");
        assert_eq!(base64(b"foobar"), "Zm9vYmFy");
    }

    #[test]
    fn base64_survives_arbitrary_bytes() {
        // A pty stream is not UTF-8 and can split a code point mid-sequence, so
        // the encoder has to be byte-clean rather than string-shaped.
        let raw: Vec<u8> = (0u8..=255).collect();
        let encoded = base64(&raw);
        assert!(encoded.is_ascii());
        assert_eq!(encoded.len(), raw.len().div_ceil(3) * 4);
    }

    #[test]
    fn string_escapes_control_characters() {
        assert_eq!(string("a\"b\\c"), "\"a\\\"b\\\\c\"");
        assert_eq!(string("l1\nl2\tx"), "\"l1\\nl2\\tx\"");
        assert_eq!(string("\u{1}"), "\"\\u0001\"");
        // Non-ASCII stays raw, which protojson also does.
        assert_eq!(string("héllo→"), "\"héllo→\"");
    }

    #[test]
    fn string_array_renders_empty_and_populated() {
        assert_eq!(string_array(&[]), "[]");
        assert_eq!(
            string_array(&["/a".to_string(), "/b".to_string()]),
            "[\"/a\", \"/b\"]"
        );
    }
}
