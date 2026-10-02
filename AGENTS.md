# ass

Agent sessions. Go index + embedded zsh launcher. macOS.

- Small functions, short names. Comments explain why. Dependencies need a reason.
- Code, documentation, comments and user-facing text must be in English. Use Unicode escapes for non-English test data.
- User-facing text: plain, short. README and help fit one screen.
- Every behavior change gets a test. Run `make test` for code changes. Before release or after dependency changes, use `make check` instead; it includes tests and cross-builds.
- Picker changes: run `make test-ui` too. It uses synthetic sessions and opens no agents.
- Keep launch flags, import identities and the existing `cs` cache paths stable.
- Source sessions are read-only. Export only the selected session; remove temporary copies.
- Cache stays plaintext: directories 0700, files 0600. Never read real sessions for tests or add them to fixtures.
- Commit only when asked. Message: `better`. No co-author trailer. Publish only when asked.
