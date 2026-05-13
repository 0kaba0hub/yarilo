# DKIM configuration

DKIM (RFC 6376) operates at the message level, independent of the transport protocol. Yarilo verifies DKIM signatures on inbound mail (MX) and signs outbound mail (submission) before relay.

---

## Top-level keys

| Key | Default | Description |
|:---|:---|:---|
| `dkim.verify` | `false` | Verify signatures on inbound MX mail. Result is passed to the DMARC evaluator. |
| `dkim.sign` | `false` | Sign outbound submission mail with the sender domain's private key. |
| `dkim.selector` | `mail` | DKIM selector (e.g. `mail` → DNS TXT `mail._domainkey.example.com`). |
| `dkim.sign_headers` | see below | Headers covered by the signature. |
| `dkim.oversign_headers` | `From` | Headers oversigned (signed one extra time) to prevent header injection. |

Default `sign_headers`: `From`, `To`, `Subject`, `Date`, `Message-ID`, `Content-Type`.

```yaml
dkim:
  verify: true
  sign: true
  selector: mail
  sign_headers:
    - From
    - To
    - Subject
    - Date
    - Message-ID
    - Content-Type
  oversign_headers:
    - From
```

---

## Key backends

### Static — PEM files on disk

```yaml
dkim:
  keys:
    backend: static
    static:
      example.com: /etc/yarilo/dkim/example.com.pem
      other.org:   /etc/yarilo/dkim/other.org.pem
```

### Dynamic — SQL database

Keys are fetched at signing time and cached for `cache_ttl` seconds. Useful for multi-domain setups managed via a database.

```yaml
dkim:
  keys:
    backend: dynamic
    dynamic:
      driver: postgres          # sqlite | mysql | postgres
      dsn: "${DKIM_DB_URL}"     # ${ENV_VAR} substitution supported
      query: "SELECT private_key FROM dkim_keys WHERE domain = $1"
      cache_ttl: 300            # seconds; 0 = no cache
```

The query must return a single column containing the RSA or Ed25519 private key in PEM format.

---

## DNS record

```
mail._domainkey.example.com. IN TXT "v=DKIM1; k=rsa; p=<base64-pubkey>"
```

Generate a key pair:

```sh
openssl genrsa -out example.com.pem 2048
openssl rsa -in example.com.pem -pubout | grep -v '^-' | tr -d '\n'
```

Paste the output as the `p=` value in the TXT record.
