#!/usr/bin/env sh
# Verifies the DNS records (CNAME + asuid TXT) required to bind a custom domain
# to an Azure Container App.
#
# Usage:
#   DOMAIN=auth.example.com \
#   RG=<resource-group> \
#   APP=<container-app-name> \
#   ./scripts/verify-dns.sh
#
# Or pass them as positional args:
#   ./scripts/verify-dns.sh <domain> <resource-group> <container-app-name>
set -e

DOMAIN="${1:-${DOMAIN:?set DOMAIN or pass as arg 1}}"
RG="${2:-${RG:?set RG or pass as arg 2}}"
APP="${3:-${APP:?set APP or pass as arg 3}}"

EXPECTED_CNAME="$(az containerapp show -g "${RG}" -n "${APP}" --query 'properties.configuration.ingress.fqdn' -o tsv)"
EXPECTED_TXT_HOST="asuid.${DOMAIN}"
EXPECTED_TXT_VALUE="$(az containerapp show -g "${RG}" -n "${APP}" --query 'properties.customDomainVerificationId' -o tsv)"

echo "Checking CNAME ${DOMAIN} -> ${EXPECTED_CNAME} ..."
CNAME_ACTUAL=$(dig +short CNAME "${DOMAIN}" @8.8.8.8 | sed 's/\.$//')
echo "  got: ${CNAME_ACTUAL:-<none>}"
if [ "${CNAME_ACTUAL}" = "${EXPECTED_CNAME}" ]; then
  echo "  OK"
else
  echo "  MISMATCH (expected ${EXPECTED_CNAME})"
fi

echo
echo "Checking TXT ${EXPECTED_TXT_HOST} -> ${EXPECTED_TXT_VALUE} ..."
TXT_ACTUAL=$(dig +short TXT "${EXPECTED_TXT_HOST}" @8.8.8.8 | tr -d '"')
echo "  got: ${TXT_ACTUAL:-<none>}"
if [ "${TXT_ACTUAL}" = "${EXPECTED_TXT_VALUE}" ]; then
  echo "  OK"
else
  echo "  MISMATCH (expected ${EXPECTED_TXT_VALUE})"
fi
