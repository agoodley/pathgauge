The dashboard is built from `web/` into `dist/` here (`make ui`) and embedded
into the binary at compile time. `dist/` is not committed; without it the
server answers dashboard requests with a "dashboard not built" message and
the REST API still works.
