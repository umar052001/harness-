# web

The dashboard: Solid, Vite and Tailwind. Built to static files and served by the
Go control plane from `web/dist`.

```sh
npm install
npm run build     # type-checks with tsc -b, then bundles to dist/
npm run dev       # vite dev server on :5173, proxying /api to :8090
```

For day-to-day work use the root `dev.sh` or `make run`, which build this and the
two other components together. See the [project README](../README.md) for what
the system does and how to run it.