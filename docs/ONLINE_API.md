# Bad Company Launcher — nobkonline.com API contract (v1)

The launcher talks to the server through the endpoints below. The server address defaults to
`https://nobkonline.com/api`. It is not shown in the UI; if needed (e.g. for a test server) it can be
changed with the `api_base` field in `launcher.json` next to the launcher.

## General rules

- Only **HTTPS** is accepted (`http://` only for `localhost` / `127.0.0.1`, for testing).
- All requests and responses are `application/json`.
- The launcher sends these headers with every request:
  - `User-Agent: BFBC-Launcher/1.0.7`
  - `X-Title-Id: 454107F9` (Bad Company's Xbox 360 title ID)
  - When logged in: `Authorization: Bearer <token>`
- On errors, return a 4xx/5xx status with this body:
  ```json
  { "error": "invalid_credentials" }
  ```
  Error codes the launcher has a translated message for: `invalid_credentials`.
  Unknown codes are shown to the user as they are.
- Request timeout: 12 seconds.

---

## `POST /v1/auth/login`

Request:
```json
{ "username": "player", "password": "••••••" }
```

Success (200):
```json
{ "token": "eyJhbGciOi...", "username": "player", "display_name": "player" }
```

- `token` is required. The launcher encrypts it with Windows DPAPI (only that Windows user can
  decrypt it) and stores it in `launcher.json`. The password is never stored.
- `display_name` is optional (`username` is shown when missing).
- Recommendation: give tokens a reasonable lifetime (e.g. 30 days) and make them revocable on the server.

## `POST /v1/auth/logout`

Header: `Authorization: Bearer <token>` · No body · Response: 200 (body ignored).
The launcher does not wait for the response; it always deletes the token locally.

## `GET /v1/status`

No authentication required.
```json
{ "online": true, "players": 42, "rooms": 5, "motd": "Weekend tournament at 21:00!" }
```

## `POST /v1/achievements/sync` (optional)

After a successful login, the launcher sends the achievements unlocked locally.
```json
{
  "title_id": "454107F9",
  "unlocked": [ { "id": 1, "filetime": 134062344000000000 } ]
}
```
`filetime`: Windows FILETIME (100 ns units since 1601-01-01). If the endpoint is missing, the launcher ignores the error.

## Web pages

Buttons in the launcher open these in the browser, using the root domain of the API address:
- **Create account** → `https://nobkonline.com/register`
- **My account** → `https://nobkonline.com/account`

---

## Information passed to the game

When **PLAY** is pressed, the launcher starts `bfbc.exe` with these environment variables.
The in-game online integration will read them:

| Variable | Value |
|---|---|
| `NOBK_MODE` | `online` or `offline` |
| `NOBK_API` | API root address |
| `NOBK_USER` | Username |
| `NOBK_TOKEN` | Session token (only in `online` mode and when logged in) |

> Note: the game's own multiplayer connection (EA back-end emulation or System Link
> tunnelling) is separate work; this contract currently covers the launcher side only.
