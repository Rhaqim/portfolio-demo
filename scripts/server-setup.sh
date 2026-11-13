#!/usr/bin/env bash
#
# server-setup.sh — bring a fresh Hetzner box to a running demo API, in one go.
#
#   git clone https://github.com/Rhaqim/portfolio-demo.git /opt/demo
#   cd /opt/demo && ./scripts/server-setup.sh
#
# It installs Docker if missing, creates .env (with a generated ADMIN_TOKEN),
# installs the Cloudflare origin certificate (it asks you to paste it the
# first time), lets only Cloudflare reach ports 80/443, gets the images (pulls
# from GHCR, or builds them here if the pull fails), starts the stack and checks
# every service through the gateway.
#
# Safe to re-run: each step checks before it acts. An existing .env or
# certificate is never overwritten. Run it again after editing .env.
#
# Options:
#   --domain HOST          API hostname (default: DEMO_DOMAIN in .env, else api.rhaqim.com)
#   --build                build the images on this box instead of pulling them
#   --deploy-user NAME     create/prepare the SSH user CI deploys as (docker group)
#   --deploy-key "KEY"     public key to authorise for that user
#   --no-firewall          skip the Cloudflare-only firewall
#   --no-swap              don't create a swap file
#   TAG=<sha> ./scripts/server-setup.sh   run a specific image tag (default: latest)
#   SSH_PORT=2222 ...      if sshd isn't on 22

set -euo pipefail

cd "$(dirname "$0")/.."
PROJECT_DIR="$(pwd)"

# ----------------------------------------------------------------------------
# Options
# ----------------------------------------------------------------------------
DOMAIN=""
FORCE_BUILD=0
WANT_FIREWALL=1
WANT_SWAP=1
DEPLOY_USER=""
DEPLOY_KEY=""
SSH_PORT="${SSH_PORT:-22}"
TAG="${TAG:-latest}"

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) DOMAIN="${2:-}"; shift ;;
	--build) FORCE_BUILD=1 ;;
	--deploy-user) DEPLOY_USER="${2:-}"; shift ;;
	--deploy-key) DEPLOY_KEY="${2:-}"; shift ;;
	--no-firewall) WANT_FIREWALL=0 ;;
	--no-swap) WANT_SWAP=0 ;;
	-h | --help)
		sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown option: $1 (try --help)" >&2
		exit 1
		;;
	esac
	shift
done

# ----------------------------------------------------------------------------
# Output helpers
# ----------------------------------------------------------------------------
if [ -t 1 ]; then
	BOLD=$(printf '\033[1m'); GREEN=$(printf '\033[32m'); YELLOW=$(printf '\033[33m')
	RED=$(printf '\033[31m'); DIM=$(printf '\033[2m'); RESET=$(printf '\033[0m')
else
	BOLD=""; GREEN=""; YELLOW=""; RED=""; DIM=""; RESET=""
fi

step() { printf '\n%s==> %s%s\n' "$BOLD" "$1" "$RESET"; }
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1"; }
warn() { printf '  %s!%s %s\n' "$YELLOW" "$RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$RED" "$RESET" "$1" >&2; }
info() { printf '  %s%s%s\n' "$DIM" "$1" "$RESET"; }
die()  { fail "$1"; exit 1; }

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || die "not running as root and sudo is not installed"
	SUDO="sudo"
fi

printf '%s\n' "${BOLD}Demo API — server setup${RESET}"
info "project: $PROJECT_DIR"
[ -f docker-compose.yml ] || die "docker-compose.yml not found — run this from the portfolio-demo checkout"

# ----------------------------------------------------------------------------
# 1. Operating system
# ----------------------------------------------------------------------------
step "Checking the operating system"
if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	ok "detected ${PRETTY_NAME:-$ID} ($(uname -m))"
	case "${ID_LIKE:-$ID}" in
	*debian* | *ubuntu*) ;;
	*) warn "this script targets Debian/Ubuntu; package installs may not work here" ;;
	esac
else
	warn "could not identify the OS; continuing anyway"
fi
for bin in curl openssl; do
	command -v "$bin" >/dev/null 2>&1 || { $SUDO apt-get update -qq && $SUDO apt-get install -y -qq "$bin" >/dev/null; }
done

# ----------------------------------------------------------------------------
# 2. Docker
# ----------------------------------------------------------------------------
step "Checking Docker"
if command -v docker >/dev/null 2>&1; then
	ok "docker present ($(docker --version | cut -d, -f1))"
else
	info "installing Docker via get.docker.com ..."
	curl -fsSL https://get.docker.com | $SUDO sh >/dev/null
	ok "docker installed"
fi

if docker compose version >/dev/null 2>&1; then
	ok "docker compose plugin present ($(docker compose version --short 2>/dev/null || echo v2))"
else
	info "installing the compose plugin ..."
	$SUDO apt-get update -qq && $SUDO apt-get install -y -qq docker-compose-plugin >/dev/null
	docker compose version >/dev/null 2>&1 || die "docker compose plugin still unavailable"
	ok "docker compose plugin installed"
fi

# Use docker without sudo when we can; otherwise sudo for this run.
DOCKER_SUDO=""
if [ -n "$SUDO" ] && ! docker info >/dev/null 2>&1; then
	if ! id -nG "$USER" | tr ' ' '\n' | grep -qx docker; then
		$SUDO usermod -aG docker "$USER" || true
		warn "added $USER to the 'docker' group — log out and back in for it to apply"
	fi
	DOCKER_SUDO="$SUDO"
fi
$DOCKER_SUDO docker info >/dev/null 2>&1 || die "docker is installed but not responding (is the daemon running?)"
ok "docker daemon responding"

dc() { $DOCKER_SUDO env TAG="$TAG" docker compose "$@"; }

# ----------------------------------------------------------------------------
# 3. Configuration (.env)
# ----------------------------------------------------------------------------
step "Checking configuration"
if [ -f .env ]; then
	ok ".env already exists (leaving it untouched)"
else
	[ -f .env.example ] || die ".env.example not found"
	cp .env.example .env
	ok "created .env from .env.example"
fi

env_get() { grep -E "^${1}=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true; }
env_set() {
	local key="$1" value="$2" tmp
	if grep -qE "^#?${key}=" .env; then
		tmp="$(mktemp)"
		sed -E "s|^#?${key}=.*|${key}=${value}|" .env >"$tmp" && cat "$tmp" >.env && rm -f "$tmp"
	else
		printf '%s=%s\n' "$key" "$value" >>.env
	fi
}

GENERATED_TOKEN=""
if [ -z "$(env_get ADMIN_TOKEN)" ]; then
	GENERATED_TOKEN="$(openssl rand -hex 32)"
	env_set ADMIN_TOKEN "$GENERATED_TOKEN"
	ok "generated ADMIN_TOKEN (shown at the end)"
else
	ok "ADMIN_TOKEN already set"
fi

[ -n "$DOMAIN" ] || DOMAIN="$(env_get DEMO_DOMAIN)"
[ -n "$DOMAIN" ] || DOMAIN="api.rhaqim.com"
env_set DEMO_DOMAIN "$DOMAIN"
ok "domain: $DOMAIN"

chmod 600 .env
ok ".env permissions set to 600"

# ----------------------------------------------------------------------------
# 4. Cloudflare origin certificate
# ----------------------------------------------------------------------------
# Caddy serves this cert; Cloudflare (SSL mode "Full (strict)") trusts it.
step "Checking the origin certificate"
CERT=certs/origin.pem
KEY=certs/origin-key.pem
mkdir -p certs

# Read one PEM block from the terminal into $1.
paste_pem() {
	local dest="$1" what="$2" line got=0
	printf '\n  Paste the %s (from -----BEGIN to -----END), then press Enter:\n' "$what"
	: >"$dest.tmp"
	while IFS= read -r line; do
		[ -z "$line" ] && [ "$got" -eq 0 ] && continue
		got=1
		printf '%s\n' "$line" >>"$dest.tmp"
		case "$line" in -----END*) break ;; esac
	done
	mv "$dest.tmp" "$dest"
}

if [ ! -s "$CERT" ] || [ ! -s "$KEY" ]; then
	if [ ! -t 0 ]; then
		fail "missing $CERT and/or $KEY"
		info "Cloudflare → SSL/TLS → Origin Server → Create certificate for $DOMAIN,"
		info "save them to $PROJECT_DIR/$CERT and $PROJECT_DIR/$KEY, then re-run."
		exit 1
	fi
	info "Cloudflare → SSL/TLS → Origin Server → Create certificate (RSA or ECC, hostname $DOMAIN)."
	(
		umask 077
		[ -s "$CERT" ] || paste_pem "$CERT" "Origin Certificate"
		[ -s "$KEY" ] || paste_pem "$KEY" "Private Key"
	)
fi
chmod 600 "$CERT" "$KEY"

openssl x509 -noout -in "$CERT" 2>/dev/null || die "$CERT is not a valid certificate — delete it and re-run"
if [ "$(openssl x509 -noout -pubkey -in "$CERT" 2>/dev/null)" != "$(openssl pkey -pubout -in "$KEY" 2>/dev/null)" ]; then
	die "$KEY does not match $CERT — delete both and re-run"
fi
ok "certificate and key present and matching (expires $(openssl x509 -noout -enddate -in "$CERT" | cut -d= -f2))"
openssl x509 -noout -checkend 2592000 -in "$CERT" >/dev/null || warn "the certificate expires within 30 days"

# ----------------------------------------------------------------------------
# 5. Swap (compiling Rust/Go on a small box needs it)
# ----------------------------------------------------------------------------
step "Checking swap"
if [ "$WANT_SWAP" -eq 0 ]; then
	info "skipped (--no-swap)"
elif [ "$(swapon --show --noheadings 2>/dev/null | wc -l)" -gt 0 ]; then
	ok "swap already configured ($(free -h | awk '/Swap:/ {print $2}'))"
else
	TOTAL_MB=$(free -m | awk '/Mem:/ {print $2}')
	AVAIL_GB=$(df -BG --output=avail / 2>/dev/null | tail -1 | tr -dc '0-9')
	if [ "${TOTAL_MB:-0}" -ge 7500 ]; then
		ok "${TOTAL_MB}MB RAM — enough without swap"
	elif [ "${AVAIL_GB:-0}" -lt 7 ]; then
		warn "only ${AVAIL_GB}GB free on / — skipping swap"
	elif { $SUDO fallocate -l 2G /swapfile 2>/dev/null || $SUDO dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none; } &&
		$SUDO chmod 600 /swapfile && $SUDO mkswap /swapfile >/dev/null && $SUDO swapon /swapfile; then
		grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' | $SUDO tee -a /etc/fstab >/dev/null
		ok "created 2GB swap (${TOTAL_MB}MB RAM) and made it permanent"
	else
		$SUDO rm -f /swapfile
		warn "could not enable swap — continuing without it"
	fi
fi

# ----------------------------------------------------------------------------
# 6. Firewall: SSH from anywhere, 80/443 from Cloudflare only
# ----------------------------------------------------------------------------
# The services rate-limit on CF-Connecting-IP, which anyone reaching the box
# directly could forge. Docker publishes ports through iptables' FORWARD chain,
# which ufw never sees, so there are two layers:
#   - ufw for traffic that ends on the host (SSH, and Docker's IPv6 proxy);
#   - a DOCKER-USER rule (Docker's hook for this) for traffic to containers,
#     re-applied by a systemd unit whenever Docker starts.
step "Configuring the firewall"
FW_OK=0
if [ "$WANT_FIREWALL" -eq 0 ]; then
	info "skipped (--no-firewall)"
else
	$SUDO mkdir -p /etc/demo
	for v in 4 6; do
		if list="$(curl -fsS --max-time 10 "https://www.cloudflare.com/ips-v$v")" &&
			printf '%s\n' "$list" | grep -qE '^[0-9a-f:.]+/[0-9]+$'; then
			printf '%s\n' "$list" | grep -E '^[0-9a-f:.]+/[0-9]+$' | $SUDO tee "/etc/demo/cloudflare-ips-v$v" >/dev/null
		elif [ -s "/etc/demo/cloudflare-ips-v$v" ]; then
			warn "could not refresh Cloudflare's IPv$v ranges; using the saved list"
		fi
	done

	if [ ! -s /etc/demo/cloudflare-ips-v4 ]; then
		warn "could not fetch Cloudflare's IP ranges; firewall NOT configured"
	else
		command -v ufw >/dev/null 2>&1 || $SUDO apt-get install -y -qq ufw >/dev/null
		# SSH first, so enabling can't lock you out.
		$SUDO ufw allow "${SSH_PORT}/tcp" >/dev/null
		$SUDO ufw default deny incoming >/dev/null
		$SUDO ufw delete allow 80/tcp >/dev/null 2>&1 || true
		$SUDO ufw delete allow 443/tcp >/dev/null 2>&1 || true
		cat /etc/demo/cloudflare-ips-v4 /etc/demo/cloudflare-ips-v6 2>/dev/null | while read -r net; do
			$SUDO ufw allow proto tcp from "$net" to any port 80,443 comment cloudflare >/dev/null || true
		done
		$SUDO ufw status | grep -q "Status: active" || $SUDO ufw --force enable >/dev/null
		ok "ufw active (SSH ${SSH_PORT} open; 80/443 from Cloudflare only)"

		$SUDO tee /usr/local/sbin/demo-cf-firewall >/dev/null <<'EOF'
#!/bin/sh
# Admit only Cloudflare to containers' published ports 80/443 (installed by
# portfolio-demo/scripts/server-setup.sh). Idempotent.
set -eu
apply() { # iptables|ip6tables  ranges-file  probe-address
	ipt=$1 file=$2
	command -v "$ipt" >/dev/null 2>&1 || return 0
	[ -s "$file" ] || return 0
	"$ipt" -n -L DOCKER-USER >/dev/null 2>&1 || return 0
	fam=-4; [ "$ipt" = ip6tables ] && fam=-6
	iface=$(ip "$fam" -o route get "$3" 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p')
	[ -n "$iface" ] || return 0
	"$ipt" -N DEMO-CF 2>/dev/null || "$ipt" -F DEMO-CF
	while read -r net; do [ -n "$net" ] && "$ipt" -A DEMO-CF -s "$net" -j RETURN; done <"$file"
	"$ipt" -A DEMO-CF -j DROP
	for port in 80 443; do
		# Only connections that start outside: replies to the containers' own
		# outbound HTTPS (e.g. to OpenAI) must pass.
		set -- -i "$iface" -p tcp -m conntrack --ctdir ORIGINAL --ctorigdstport "$port" -j DEMO-CF
		"$ipt" -C DOCKER-USER "$@" 2>/dev/null || "$ipt" -I DOCKER-USER "$@"
	done
}
apply iptables /etc/demo/cloudflare-ips-v4 1.1.1.1
apply ip6tables /etc/demo/cloudflare-ips-v6 2606:4700:4700::1111
EOF
		$SUDO chmod 755 /usr/local/sbin/demo-cf-firewall
		$SUDO tee /etc/systemd/system/demo-cf-firewall.service >/dev/null <<'EOF'
[Unit]
Description=Admit only Cloudflare to the demo's ports 80/443
After=docker.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/demo-cf-firewall

[Install]
WantedBy=docker.service
EOF
		$SUDO systemctl daemon-reload
		$SUDO systemctl enable demo-cf-firewall.service >/dev/null 2>&1
		$SUDO systemctl restart demo-cf-firewall.service
		if $SUDO iptables -n -L DEMO-CF >/dev/null 2>&1; then
			ok "Docker ports 80/443 restricted to Cloudflare (re-applied on every Docker start)"
			FW_OK=1
		else
			warn "DOCKER-USER rule not applied — check: systemctl status demo-cf-firewall"
		fi
	fi
fi

# ----------------------------------------------------------------------------
# 7. CI deploy user (optional)
# ----------------------------------------------------------------------------
if [ -n "$DEPLOY_USER" ]; then
	step "Preparing deploy user '$DEPLOY_USER'"
	id "$DEPLOY_USER" >/dev/null 2>&1 || { $SUDO useradd -m -s /bin/bash "$DEPLOY_USER" && ok "created user"; }
	$SUDO usermod -aG docker "$DEPLOY_USER"
	ok "in the docker group"
	# CI copies these two files in on each deploy.
	$SUDO chown "$DEPLOY_USER" docker-compose.yml Makefile
	ok "can update docker-compose.yml and Makefile"
	if [ -n "$DEPLOY_KEY" ]; then
		HOME_DIR="$(getent passwd "$DEPLOY_USER" | cut -d: -f6)"
		$SUDO install -d -m 700 -o "$DEPLOY_USER" "$HOME_DIR/.ssh"
		$SUDO touch "$HOME_DIR/.ssh/authorized_keys"
		$SUDO grep -qxF "$DEPLOY_KEY" "$HOME_DIR/.ssh/authorized_keys" ||
			printf '%s\n' "$DEPLOY_KEY" | $SUDO tee -a "$HOME_DIR/.ssh/authorized_keys" >/dev/null
		$SUDO chown "$DEPLOY_USER" "$HOME_DIR/.ssh/authorized_keys"
		$SUDO chmod 600 "$HOME_DIR/.ssh/authorized_keys"
		ok "deploy key authorised"
	fi
fi

# ----------------------------------------------------------------------------
# 8. Images
# ----------------------------------------------------------------------------
step "Getting images (tag: $TAG)"
SERVICES="gateway hub buckt loom ussd"
BUILT=0
if [ "$FORCE_BUILD" -eq 0 ] && dc pull --quiet 2>/dev/null; then
	ok "pulled from $(env_get REGISTRY)"
else
	[ "$FORCE_BUILD" -eq 1 ] || warn "pull failed (no CI run yet, private packages, or wrong arch) — building here"
	info "one at a time to stay within memory; the Rust (ussd) build is the slow one"
	for svc in $SERVICES; do
		printf '  … building %s\n' "$svc"
		dc build "$svc" >/dev/null 2>&1 || die "$svc failed to build — run 'docker compose build $svc' to see why"
		ok "$svc built"
	done
	BUILT=1
fi

# ----------------------------------------------------------------------------
# 9. Start
# ----------------------------------------------------------------------------
step "Starting the stack"
dc up -d --remove-orphans
$DOCKER_SUDO docker image prune -f >/dev/null
ok "containers started"

# ----------------------------------------------------------------------------
# 10. Health checks (through Caddy on this box, like the CI smoke test)
# ----------------------------------------------------------------------------
step "Verifying services"
HEALTHY=1
for path in api/health api/ussd/health api/buckt/health api/loom/health; do
	tries=20
	until curl -fsSk --max-time 3 --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/$path" >/dev/null 2>&1; do
		tries=$((tries - 1))
		[ "$tries" -gt 0 ] || break
		sleep 3
	done
	if [ "$tries" -gt 0 ]; then ok "/$path"; else fail "/$path did not come up"; HEALTHY=0; fi
done
[ "$HEALTHY" -eq 1 ] || { dc ps; info "logs: docker compose logs --tail 50 <service>"; }

# ----------------------------------------------------------------------------
# 11. Summary
# ----------------------------------------------------------------------------
PUBLIC_IP="$(ip -4 -o route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([^ ]*\).*/\1/p')"

printf '\n%s────────────────────────────────────────────────────────%s\n' "$BOLD" "$RESET"
if [ "$HEALTHY" -eq 1 ]; then
	printf '%s Setup complete.%s\n' "$GREEN$BOLD" "$RESET"
else
	printf '%s Setup finished with problems — see the failures above.%s\n' "$YELLOW$BOLD" "$RESET"
fi
printf '%s────────────────────────────────────────────────────────%s\n\n' "$BOLD" "$RESET"

printf '  API:          %shttps://%s%s   (Cloudflare DNS: %sapi%s → %s, proxied)\n' \
	"$BOLD" "$DOMAIN" "$RESET" "$BOLD" "$RESET" "${PUBLIC_IP:-<server ip>}"
if [ -n "$GENERATED_TOKEN" ]; then
	printf '  ADMIN_TOKEN:  %s%s%s   %s<- for reading feedback; stored in .env%s\n' "$BOLD" "$GENERATED_TOKEN" "$RESET" "$DIM" "$RESET"
fi
[ "$BUILT" -eq 1 ] && printf '  Images:       built on this box; CI deploys will replace them with pulled ones\n'
[ "$WANT_FIREWALL" -eq 1 ] && [ "$FW_OK" -eq 0 ] && printf '  %sFirewall:     not fully applied — use a Hetzner Cloud Firewall with Cloudflare ranges%s\n' "$YELLOW" "$RESET"

printf '\n  %sFor CI deploys (GitHub → Settings → Secrets and variables → Actions)%s\n' "$BOLD" "$RESET"
printf '    DEPLOY_HOST         %s\n' "${PUBLIC_IP:-<server ip>}"
printf '    DEPLOY_USER         %s\n' "${DEPLOY_USER:-<a user in the docker group; see --deploy-user>}"
printf '    DEPLOY_SSH_KEY      the private half of the deploy key\n'
if [ -n "$PUBLIC_IP" ] && [ -r /etc/ssh/ssh_host_ed25519_key.pub ]; then
	printf '    DEPLOY_KNOWN_HOSTS  %s %s\n' "$PUBLIC_IP" "$(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)"
fi
[ "$PROJECT_DIR" = /opt/demo ] || printf '    %svariable DEPLOY_DIR = %s%s   (CI defaults to /opt/demo)\n' "$YELLOW" "$PROJECT_DIR" "$RESET"
[ "$(uname -m)" = aarch64 ] && printf '    %svariable PLATFORM = linux/arm64%s   (this is an Arm box)\n' "$YELLOW" "$RESET"

printf '\n  Useful: make server-logs   make health   make backup   ./scripts/server-setup.sh (re-run any time)\n\n'

[ "$HEALTHY" -eq 1 ] || exit 1
