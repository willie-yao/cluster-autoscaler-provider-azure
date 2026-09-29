#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Checks the fixture's NSG rule, Calico mode and network probe decisions
# without contacting Azure or a cluster.

set -o errexit
set -o nounset
set -o pipefail

script=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixture.sh
env_file=$(dirname "$script")/fixture.env.example
OPERATOR_CIDR=192.0.2.1/32

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/fixture-test.XXXXXX")
trap 'rm -f "$tmp_dir/policy.env" "$tmp_dir/calico.yaml" "$tmp_dir/kubectl" "$tmp_dir/probe-cleaned"; rmdir "$tmp_dir"' EXIT
sed 's/^POLICY_NAME_PREFIX=.*/POLICY_NAME_PREFIX=policy-rule-/' "$env_file" > "$tmp_dir/policy.env"

allowed_nsg_rules() {
    "$BASH" "$script" "$tmp_dir/policy.env" self-check-nsg
}

rules=$(jq -n --arg cidr "$OPERATOR_CIDR" '[
    {name:"operator-ssh-api",direction:"Inbound",access:"Allow",
     protocol:"Tcp",priority:100,sourceAddressPrefix:$cidr,
     destinationPortRanges:["22","6443"]},
    {name:"policy-rule-103",direction:"Inbound",access:"Allow",
     sourceAddressPrefix:"203.0.113.0/24",priority:103,
     destinationPortRange:"*"},
    {name:"policy-rule-104",direction:"Inbound",access:"Allow",
     sourceAddressPrefix:"198.51.100.0/24",priority:104,
     destinationPortRange:"*"},
    {name:"policy-rule-new",direction:"Inbound",access:"Allow",
     sourceAddressPrefix:"Internet",priority:99,
     destinationPortRange:"*"},
    {name:"internal-route",direction:"Inbound",access:"Allow",
     sourceAddressPrefix:"VirtualNetwork",destinationPortRange:"443"},
    {name:"public-deny",direction:"Inbound",access:"Deny",
     sourceAddressPrefix:"Internet",destinationPortRange:"22"}
]')
allowed_nsg_rules <<< "$rules" ||
    { echo "The operator, organization policy and VNet rules should pass" >&2; exit 1; }
if "$BASH" "$script" "$env_file" self-check-nsg <<< "$rules"; then
    echo "The NSG check accepted policy rules without a policy prefix" >&2
    exit 1
fi

for changed in \
    'map(select(.name != "operator-ssh-api"))' \
    '. + [.[0]]' \
    'map(if .name == "operator-ssh-api" then .sourceAddressPrefix = "0.0.0.0/0" else . end)' \
    'map(if .name == "operator-ssh-api" then .destinationPortRanges = ["22","6443","443"] else . end)' \
    'map(if .name == "operator-ssh-api" then .protocol = "*" else . end)' \
    '. + [{name:"unapproved-rule",direction:"Inbound",access:"Allow",
           sourceAddressPrefix:"Internet",destinationPortRange:"*"}]' \
    '. + [{name:"unapproved-rule",direction:"Inbound",access:"Allow",
           sourceAddressPrefix:"203.0.113.0/24",destinationPortRange:"*"}]'; do
    if allowed_nsg_rules <<< "$(jq "$changed" <<< "$rules")"; then
        echo "The NSG check accepted an unsafe inbound rule" >&2
        exit 1
    fi
done

calico_manifest() {
    cat > "$tmp_dir/calico.yaml" <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: calico-node
spec:
  template:
    spec:
      containers:
      - name: calico-node
        env:
        - name: CALICO_IPV4POOL_VXLAN
          value: $1
        - name: CALICO_IPV4POOL_IPIP
          value: $2
EOF
}
calico_manifest Always Never
"$BASH" "$script" "$env_file" self-check-calico "$tmp_dir/calico.yaml" ||
    { echo "Calico Always and Never should pass" >&2; exit 1; }
for mode in "CrossSubnet Never" "Always Always" "Never Never"; do
    read -r vxlan ipip <<< "$mode"
    calico_manifest "$vxlan" "$ipip"
    if output=$("$BASH" "$script" "$env_file" self-check-calico "$tmp_dir/calico.yaml" 2>&1); then
        echo "The Calico check accepted VXLAN=$vxlan and IPIP=$ipip" >&2
        exit 1
    fi
    [[ $output = *"CALICO_IPV4POOL_VXLAN=Always"* &&
        $output = *"CALICO_IPV4POOL_IPIP=Never"* ]] ||
        { echo "The Calico error must name both required modes" >&2; exit 1; }
done
calico_manifest Always Never
yq eval -i 'del(.spec.template.spec.containers[0].env[0])' "$tmp_dir/calico.yaml"
if output=$("$BASH" "$script" "$env_file" self-check-calico "$tmp_dir/calico.yaml" 2>&1); then
    echo "The Calico check accepted a missing VXLAN mode" >&2
    exit 1
fi
[[ $output = *"CALICO_IPV4POOL_VXLAN=Always"* &&
    $output = *"CALICO_IPV4POOL_IPIP=Never"* ]] ||
    { echo "The Calico error must name both required modes" >&2; exit 1; }

cat > "$tmp_dir/kubectl" <<'EOF'
#!/usr/bin/env bash
case " $* " in
    *" exec "*) exec sleep 10 ;;
    *" get namespace "*) printf '%s\n' '{"metadata":{"labels":{"autoscaler-e2e-run":"fixture-test"}}}' ;;
    *" delete namespace "*) : > "$FAKE_KUBECTL_LOG" ;;
    *) exit 1 ;;
esac
EOF
chmod +x "$tmp_dir/kubectl"
start=$SECONDS
if output=$(FAKE_KUBECTL_LOG="$tmp_dir/probe-cleaned" PATH="$tmp_dir:$PATH" \
    "$BASH" "$script" "$env_file" self-check-probe "$tmp_dir" 2>&1); then
    echo "A stalled Pod exec should fail" >&2
    exit 1
fi
(( SECONDS - start < 6 )) ||
    { echo "The stalled Pod exec exceeded its local timeout" >&2; exit 1; }
[[ -f $tmp_dir/probe-cleaned && ! -e $tmp_dir/network-probe-response &&
    $output = *"Pod-IP HTTP"* ]] ||
    { echo "The stalled Pod exec did not clean up its namespace" >&2; exit 1; }

echo "Fixture NSG rule self-check passed"
echo "Fixture Calico mode self-check passed"
echo "Fixture network probe timeout self-check passed"
