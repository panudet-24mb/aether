# PostgreSQL for the production stack, plus pgBackRest for continuous WAL archiving (point-in-time recovery).
#
# Same base as before (the official Alpine image), so the C library and therefore text collation of an existing data
# directory do not change. The base is pinned by digest and the packages by exact version: bump them deliberately.
# A pin that Alpine has since replaced fails the build loudly instead of installing something else.
ARG PG_BASE=postgres:18-alpine@sha256:d3e1620b530c944afa6e887d22eb899824da68e19c52024bf98f5220c88a65b2
FROM ${PG_BASE}

# pgBackRest 2.55 is the first release that supports PostgreSQL 18. curl posts ops alerts with the webhook URL read
# from a config on stdin, so the URL never appears in a process argument list.
RUN apk add --no-cache pgbackrest=2.58.0-r0 curl=8.22.0-r0 \
 && pgbackrest version \
 && install -d -o postgres -g postgres -m 0750 /var/lib/pgbackrest /var/spool/pgbackrest \
 && install -d -o postgres -g postgres -m 0775 /run/aether-ops

# archive_command wrapper: pgbackrest archive-push, plus a flag file when the queue cap made it DROP a segment.
COPY --chmod=0755 archive-push.sh /usr/local/bin/aether-pgbackrest-push
