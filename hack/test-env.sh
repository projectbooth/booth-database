#!/usr/bin/env sh
# Prints the environment variables that point the test suite at hack/docker-compose.yml's
# PostgreSQL. Usage: eval "$(sh hack/test-env.sh)"
cat <<'VARS'
export BOOTH_TEST_POSTGRES_ADMIN_DSN=postgres://booth_admin:booth-admin-test@127.0.0.1:15442/postgres?sslmode=disable
VARS
