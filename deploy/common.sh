# Shared helpers for the deploy and provision scripts. Source it; do not run it.
#
# Node access values are read from the environment (see deploy/remote/node.env.example),
# never from this file, so nothing here may contain a real address, port, key, or user.

set -euo pipefail

die() {
	echo "error: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# Region for every AWS call in the deploy scripts. Override with SQSH_REGION.
REGION="${SQSH_REGION:-eu-central-1}"

# Resource names derived from HOST_ID. The topic and results table are shared
# by every host; the queue and DLQ are per host.
TOPIC_NAME="sqsh-commands"
RESULTS_TABLE="sqsh-results"

require_host_id() {
	[ -n "${1:-}" ] || die "HOST_ID is required"
}
