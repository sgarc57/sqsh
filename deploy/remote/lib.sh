# Helpers for the remote-node scripts. Source it; do not run it.
#
# Every node address, port, key, and user comes from the environment (loaded from
# deploy/remote/node.env by the Makefile, or exported by hand). Nothing here is a literal.

source "$(dirname "${BASH_SOURCE[0]}")/../common.sh"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

need ssh
need scp

for v in HOST_ID NODE_SSH NODE_PORT NODE_KEY NODE_USER NODE_HOME; do
	[ -n "${!v:-}" ] || die "$v is not set; see deploy/remote/node.env.example"
done

SSH_OPTS=(-p "$NODE_PORT" -i "$NODE_KEY" -o BatchMode=yes -o ConnectTimeout=10)

# Run a command on the node.
remote() {
	ssh "${SSH_OPTS[@]}" "$NODE_USER@$NODE_SSH" "$@"
}

# Copy a local file to a path on the node.
copy_to_node() {
	scp -P "$NODE_PORT" -i "$NODE_KEY" -o BatchMode=yes -q "$1" "$NODE_USER@$NODE_SSH:$2"
}

# Fail early, with a clear message, if the node cannot run sudo without a password.
require_node_sudo() {
	remote 'sudo -n true' 2>/dev/null || die "passwordless sudo is required for $NODE_USER on the node"
}

# Look up the AWS identifiers the node config needs. Fails if `make provision` has not run.
resolve_aws() {
	local out
	if ! out=$(aws --region "$REGION" sqs get-queue-url --queue-name "sqsh-cmd-$HOST_ID" \
		--query QueueUrl --output text 2>&1); then
		die "queue for $HOST_ID not found; run make provision first: $out"
	fi
	QUEUE_URL="$out"

	TOPIC_ARN=$(aws --region "$REGION" sns list-topics --output json |
		jq -r --arg n "$TOPIC_NAME" '[.Topics[].TopicArn | select(endswith(":" + $n))][0] // empty')
	[ -n "$TOPIC_ARN" ] || die "topic $TOPIC_NAME not found; run make provision first"
}

# Render the node config into the file $2, with execute set to $1 (true or false).
render_config() {
	sed -e "s|@HOST_ID@|$HOST_ID|g" \
		-e "s|@REGION@|$REGION|g" \
		-e "s|@TOPIC_ARN@|$TOPIC_ARN|g" \
		-e "s|@QUEUE_URL@|$QUEUE_URL|g" \
		-e "s|@EXECUTE@|$1|g" \
		"$ROOT/deploy/config.remote.yaml.tmpl" >"$2"
}

# Render the systemd unit into the file $1.
render_unit() {
	sed -e "s|@NODE_USER@|$NODE_USER|g" \
		-e "s|@NODE_HOME@|$NODE_HOME|g" \
		"$ROOT/deploy/sqshd.service" >"$1"
}

# Install the binary, config, and unit on the node. $1 is the execute value (true|false).
# Stages through a temp dir on this machine and a temp dir on the node, and removes both.
install_node() {
	local execute=$1
	local stage
	stage=$(mktemp -d)

	render_config "$execute" "$stage/config.yaml"
	render_unit "$stage/sqshd.service"

	remote 'mkdir -p ~/sqsh-stage'
	copy_to_node "$ROOT/bin/linux-arm64/sqshd" "sqsh-stage/sqshd"
	copy_to_node "$stage/config.yaml" "sqsh-stage/config.yaml"
	copy_to_node "$stage/sqshd.service" "sqsh-stage/sqshd.service"
	rm -rf "$stage"

	remote 'set -e
		sudo install -m 0755 ~/sqsh-stage/sqshd /usr/local/bin/sqshd
		sudo install -d -m 0755 /etc/sqshd
		sudo install -m 0644 ~/sqsh-stage/config.yaml /etc/sqshd/config.yaml
		sudo install -m 0644 ~/sqsh-stage/sqshd.service /etc/systemd/system/sqshd.service
		sudo systemctl daemon-reload
		rm -rf ~/sqsh-stage'
	echo "installed on node (execute: $execute)"
}
