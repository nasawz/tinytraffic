#!/bin/bash
# Renews the Let's Encrypt certificate when it is within 30 days of expiry,
# and reloads nginx only if a new certificate was issued.
#
#   renew-cert.sh            renew if due (what the timer runs)
#   renew-cert.sh --dry-run  rehearse against Let's Encrypt's staging server
#   renew-cert.sh --force    renew now, e.g. to test the whole path
#
# Output goes to the journal (journalctl -u tinytraffic-renew) and to
# /var/log/tinytraffic-renew.log.
set -euo pipefail

DOMAIN="${TT_DOMAIN:-tinytraffic.xiangshikeji.com}"
LOG=/var/log/tinytraffic-renew.log
CERT="/etc/letsencrypt/live/$DOMAIN/fullchain.pem"

log() { echo "$(date '+%F %T') $*" | tee -a "$LOG"; }

case "${1:-}" in
  --dry-run) extra=(--dry-run) ;;
  --force)   extra=(--force-renewal) ;;
  "")        extra=() ;;
  *) echo "usage: $0 [--dry-run|--force]" >&2; exit 2 ;;
esac

log "renew: checking $DOMAIN ${extra[*]:-}"
# The deploy hook runs only when a certificate was really renewed.
if ! certbot renew --quiet --cert-name "$DOMAIN" "${extra[@]}" \
    --deploy-hook 'nginx -t && systemctl reload nginx' 2>&1 | tee -a "$LOG"; then
  log "renew: FAILED"
  exit 1
fi

if [ -r "$CERT" ]; then
  end=$(openssl x509 -enddate -noout -in "$CERT" | cut -d= -f2)
  days=$(( ( $(date -d "$end" +%s) - $(date +%s) ) / 86400 ))
  log "renew: ok, certificate valid for $days more days (until $end)"
  # A certificate that is still short after a run means renewal is broken.
  if [ "$days" -lt 14 ]; then
    log "renew: WARNING certificate expires in $days days"
    exit 1
  fi
fi
