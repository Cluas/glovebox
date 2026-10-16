# Security policy

Glovebox runs as root on a network gateway, so security reports are taken seriously.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's "Report a vulnerability" button
(Security → Advisories) rather than in public issues. Include the version (`glovebox version`),
the device and firmware, and steps to reproduce. You should get a response within a week.

## Threat model

- The console is meant to be reached only from the device's own hotspot LAN. Requests from
  outside `allow_cidrs` are rejected before authentication.
- Anyone who knows the console password can reboot the device, change Wi-Fi credentials, switch
  proxies and read logs. Choose a strong password.
- Passwords are stored as PBKDF2-SHA256 hashes in `/data/adb/glovebox/config.json` (mode 0600,
  directory 0700). Sessions are HMAC-signed cookies; changing the password revokes all sessions.
  Repeated failed logins trigger an exponential lockout.
- Non-GET API requests must carry `X-Glovebox: 1`, which cross-site forms cannot send. Responses
  set a strict Content-Security-Policy, `X-Frame-Options: DENY` and `nosniff`.
- The generated local CA is name-constrained to the configured hostnames and listen IPs. Its
  private key never leaves the device. Verify the fingerprint shown in the UI before trusting it.
- Glovebox never stores the vendor admin password; it reads the vendor's own login hash from the
  vendor database on the device.

## Supported versions

Only the latest release receives fixes.
