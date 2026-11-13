# demo-blog

One public article that signed-in people can edit. A small app for trying out
[Lift](https://lift.kabob.io): about 12 MB as an image and 4 MB of memory when idle.

- **Reading** is public. No account, no login.
- **Editing** needs "Sign in with Kabob", which goes through Lift (OpenID Connect).
  Who may sign in is set on the app's page in Lift; anyone who can sign in can edit.
- The article is a JSON file on the `/data` volume. Sessions are signed cookies, so
  nothing is lost when Lift puts the app to sleep.

## Deploy on Lift

1. Create an app with this repo's URL, branch `main`, port `8080`, and a persistent volume.
2. On the app's page, under **Sign in with Kabob**, add who may sign in.
3. Press **Deploy**.

Lift provides everything the app needs to sign people in: `PORT`, `APP_URL`,
`LIFT_ISSUER`, `LIFT_CLIENT_ID` and `LIFT_CLIENT_SECRET`.

## Settings

| Variable | Set by | Purpose |
|---|---|---|
| `PORT` | Lift | Port to listen on (default `8080`) |
| `APP_URL` | Lift | The app's public address, used for the login callback |
| `LIFT_ISSUER`, `LIFT_CLIENT_ID`, `LIFT_CLIENT_SECRET` | Lift | Where, as what, and with which secret to sign in |
| `SESSION_SECRET` | optional | Cookie signing key; derived from the client secret when unset |
| `DATA_DIR` | optional | Where the article is stored (default `/data`) |

Without `LIFT_CLIENT_SECRET` (running outside Lift) the blog still serves the article;
the sign-in link is hidden.

## Run locally

```bash
docker build -t demo-blog .
docker run --rm -p 8080:8080 -e APP_URL=http://localhost:8080 -v demo-blog:/data demo-blog
```

To sign in locally, add `http://localhost:8080/auth/callback` to the app's allowed
redirect URIs in Lift, get a secret with **Rotate** on the app's page, and pass
`LIFT_ISSUER`, `LIFT_CLIENT_ID` and `LIFT_CLIENT_SECRET`.

## How the login works

`main.go`, in order: `/auth/login` sends the browser to Lift with a state, nonce and PKCE
challenge kept in a short-lived signed cookie. `/auth/callback` checks the state, swaps the
code for an ID token, verifies it against Lift's keys, and stores the person's id, name and
email in a signed session cookie. `POST /edit` refuses anything without a valid session.
