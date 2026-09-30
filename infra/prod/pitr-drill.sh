#!/bin/sh
# Monthly point-in-time recovery drill: restores the live database to N minutes ago (default 10) into a throwaway
# volume, checks it (promotion, migration version, forced RLS, rows of core.sensor_samples in the hour before the
# target equal in both copies), appends PASS/FAIL to <backup-dir>/drills.log and removes the copy. The live
# database is only read. Exit status 0 on PASS.
#   infra/prod/pitr-drill.sh [--minutes N] [--env-file FILE] [--project NAME]
# DRILL_TABLE / DRILL_COLUMN choose another time-stamped table.
exec sh "$(cd "$(dirname "$0")" && pwd)/restore-pitr.sh" --drill "$@"
