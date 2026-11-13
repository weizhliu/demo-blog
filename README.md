# demo-blog

One public article that signed-in people can edit. A small app for trying out
[Lift](https://lift.kabob.io): about 12 MB as an image and 4 MB of memory when idle.

- **Reading** is public. No account, no login.
- **Editing** needs "Sign in with Kabob", which goes through Lift (OpenID Connect).
  Who may sign in is set on the app's page in Lift; anyone who can sign in can edit.
- The article is a JSON file on the `/data` volume. Sessions are signed cookies, so
  nothing is lost when Lift puts the app to sleep.

## Deploy on Lift

1. Create an app with source **Git repository**, this repo's URL, branch `main`,
   port `8080`, and a persistent volume.
2. On the app's page, under **Sign in with Kabob**: add who may sign in, then
   **Generate secret**.
3. Under **Environment variables**, set `LIFT_CLIENT_SECRET` to that secret.
4. Press **Deploy**.

Lift provides `PORT`, `APP_URL`, `LIFT_ISSUER` and `LIFT_CLIENT_ID` itself.

## Settings

| Variable | Set by | Purpose |
|---|---|---|
| `PORT` | Lift | Port to listen on (default `8080`) |
| `APP_URL` | Lift | The app's public address, used for the login callback |
| `LIFT_ISSUER`, `LIFT_CLIENT_ID` | Lift | Where and as what to sign in |
| `LIFT_CLIENT_SECRET` | you | Generated on the app's page in Lift |
| `SESSION_SECRET` | optional | Cookie signing key; derived from the client secret when unset |
| `DATA_DIR` | optional | Where the article is stored (default `/data`) |

Without `LIFT_CLIENT_SECRET` the blog still serves the article; the sign-in link is hidden.

## Run locally

```bash
docker build -t demo-blog .
docker run --rm -p 8080:8080 -e APP_URL=http://localhost:8080 -v demo-blog:/data demo-blog
```

To sign in locally, add `http://localhost:8080/auth/callback` to the app's allowed
redirect URIs in Lift and pass `LIFT_ISSUER`, `LIFT_CLIENT_ID` and `LIFT_CLIENT_SECRET`.

## How the login works

`main.go`, in order: `/auth/login` sends the browser to Lift with a state, nonce and PKCE
challenge kept in a short-lived signed cookie. `/auth/callback` checks the state, swaps the
code for an ID token, verifies it against Lift's keys, and stores the person's id, name and
email in a signed session cookie. `POST /edit` refuses anything without a valid session.
