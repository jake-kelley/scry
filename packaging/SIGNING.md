# Code signing scry, and why it isn't optional

Design doc §9 gotcha 6.

## The problem

macOS's TCC (Transparency, Consent, and Control — the system behind the
Documents/Desktop/Downloads/Full Disk Access prompts) grants access **keyed
to the app's code signing identity**, not to its path or bundle ID alone.

Ad-hoc signing (`codesign --sign -`, what you get if you sign nothing
explicitly) derives that identity **from a hash of the binary itself**. Every
time you `go build`, the binary's bytes change, so its ad-hoc identity
changes, so **macOS sees a brand-new, never-before-seen app** — the TCC
grants you gave the previous build don't carry over. If scry has roots under
`~/Documents`, every single rebuild during development means clicking through
the Documents permission prompt again. That's not a rare edge case here; it's
the normal edit-build-run loop turned into a permission-prompt treadmill.

The fix is a **self-signed code signing certificate that doesn't change**.
Sign every build with the same certificate, and the identity — and the TCC
grants tied to it — stays stable across rebuilds. It's still not an
Apple-issued identity (Gatekeeper will still flag the app as being from an
unidentified developer, same as ad-hoc), but it solves the TCC churn
specifically, which is the thing that actually hurts during development.

## Creating the certificate (once)

Apple doesn't support generating a code-signing-trusted certificate purely
from the CLI — `codesign` requires the certificate to carry the Code Signing
extended key usage (OID `1.3.6.1.5.5.7.3.3`) and land in a keychain codesign
will search, and the only tool that reliably sets that up is Keychain
Access's Certificate Assistant. A hand-rolled `openssl` certificate can look
right and still get silently rejected by `codesign` at sign time. Do this
once, via the GUI:

1. Open **Keychain Access** (`open -a "Keychain Access"`).
2. Menu bar: **Keychain Access → Certificate Assistant → Create a
   Certificate…**
3. Fill in:
   - **Name**: `scry-codesign` (must match exactly — `build-app.sh` and
     `install.sh` look for this name; override with `SCRY_SIGN_IDENTITY` if
     you use a different one)
   - **Identity Type**: Self Signed Root
   - **Certificate Type**: **Code Signing**
   - Check **"Let me override defaults"** if you want a longer expiry than
     the default 1 year; otherwise leave defaults and click through.
4. When prompted for a keychain, choose **login** (the default) — this is
   what "in the login keychain" means; it's per-user and doesn't need admin.
5. Finish the assistant.

Verify it's usable for signing:

```sh
security find-identity -v -p codesigning login.keychain
```

You should see a line containing `scry-codesign`. If it's missing, the
certificate either landed in the wrong keychain or wasn't marked as a Code
Signing type — redo step 3.

The first time you sign with a fresh self-signed certificate, macOS may pop
a "codesign wants to sign using key … in your keychain" prompt. Click
**Always Allow** so `build-app.sh` can sign non-interactively afterward.

## Signing with it

`packaging/build-app.sh` does this automatically: it looks for an identity
named `scry-codesign` (or `$SCRY_SIGN_IDENTITY` if set) via
`security find-identity`, and if found:

```sh
codesign --force --deep --options runtime --sign "scry-codesign" scry.app
```

If no such identity exists, it falls back to ad-hoc signing
(`codesign --force --deep --sign -`) and prints a warning — the app still
runs, you'll just be back on the permission-prompt treadmill above.

## Checking what you got

```sh
codesign -dv --verbose=4 /Applications/scry.app
```

Look at the `Authority=` line: `scry-codesign` means the stable identity is
in use; no `Authority=` line (or `Signature=adhoc`) means ad-hoc.

```sh
codesign --verify --deep --strict /Applications/scry.app
```

exits 0 for a well-formed signature either way — it checks the signature is
internally consistent, not who signed it.
