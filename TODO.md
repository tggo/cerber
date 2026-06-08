# TODO / backlog

Small, non-urgent improvements. Promote to a `DEFINITION_OF_DONE.md` entry when picked up.

- [x] **Favicon for the dashboard** — embedded SVG served at `/favicon.svg` + `/favicon.ico`, linked from `dashboard.html` (done 2026-06-08).
- [ ] **Dashboard "Add Claude account" (OAuth login)** — deferred 2026-06-08. Manual-code flow (remote proxy): `/admin/claude-login/start` → authorize URL → paste code → `/admin/claude-login/finish` → Exchange → save to auth_dir + add to live pool. Needs `credential.Store.Add` (live add) + in-memory pending-state (TTL). Closes PARITY "Add account via UI".
