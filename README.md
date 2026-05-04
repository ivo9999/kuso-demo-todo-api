# kuso-demo-todo-api

Tiny Go HTTP service backing the kuso demo todo app. Exposes JSON CRUD over a Postgres table.

## Routes

- `GET    /healthz`           – liveness probe
- `GET    /api/todos`         – list (newest first, max 200)
- `POST   /api/todos`         – `{ "title": "..." }`
- `PATCH  /api/todos/:id`     – `{ "done": true|false }`
- `DELETE /api/todos/:id`

## Env

- `DATABASE_URL` (required) – Postgres DSN. Auto-injected by kuso when a Postgres addon is attached to the project.
- `PORT` (default `8080`)

## Deploy via kuso

1. Create a project in kuso.
2. Add a Postgres addon to it.
3. Add this repo as a service. The runtime is auto-detected (nixpacks → Go).
4. Reference the addon DSN as `DATABASE_URL=${{ <addon>.DATABASE_URL }}`.
