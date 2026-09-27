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

# Creates, checks and removes one disposable Azure VMSS fixture for the E2E
# suites in ../suites, and runs one case at a time against it. The operator
# steps are described in ../README.md.

set -o errexit
set -o nounset
set -o pipefail
umask 077

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly HERE
REPO_ROOT=$(cd "$HERE/../../../.." && pwd)
readonly REPO_ROOT
readonly MODULE_DIR=$REPO_ROOT/cloudprovider/azure/test

fail() { echo "$*" >&2; exit 1; }
note() { printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
usage() {
    cat >&2 <<'EOF'
Usage: fixture.sh RUN.env COMMAND [ARG]
Commands: check, image, infra, tunnel {ssh|api}, up, cluster, pools,
  ca-deploy, ca-start, ca-stop, phase NAME, dra {up|down}, disk {up|down},
  signal {start|missing|spot}, run CASE-ID, watch, down
EOF
}
# Exports each KEY=value line of env file $1. Blank lines and comments are
# skipped. An unknown key or a line without "=" stops the script.
load_env() {
    local line key value
    [[ -f $1 ]] || fail "Fixture environment file does not exist"
    while IFS= read -r line || [[ -n $line ]]; do
        case $line in ''|\#*) continue ;; esac
        [[ $line = *=* ]] || fail "Invalid fixture environment line"
        key=${line%%=*}
        value=${line#*=}
        case $key in
            RUN_ID|PREFIX|SUBSCRIPTION|LOCATION|ZONE|DEADLINE|OPERATOR_CIDR|IMAGE|CA_IMAGE|\
            WORKLOAD_IMAGE|PRIVATE_DIR|REPORT_DIR|CALICO_MANIFEST|CALICO_SHA256|CCM_CHART|\
            DISK_CHART|VNET_CIDR|CP_CIDR|WORKER_CIDR|BASTION_CIDR|POD_CIDR|DEMAND_CPU|\
            LARGE_CPU|SSH_PORT|API_PORT|RUN_BUDGET_USD|CLEANUP_RESERVE_USD|\
            STOP_NEW_HOURS|PAST_RUN_COST_USD|RUN_ATTEMPT|POLICY_NAME_PREFIX)
                export "$key=$value" ;;
            *) fail "Unknown fixture environment field: $key" ;;
        esac
    done < "$1"
    unset POOL_TAINT
}
# Stops the script unless every variable named in the arguments is set.
required() {
    local name
    for name in "$@"; do
        [[ -n $(printenv "$name" 2>/dev/null) ]] || fail "$name is required"
    done
}
# Prints the Unix time of UTC timestamp $1 with GNU or BSD date.
epoch() {
    if date -u -d "$1" +%s >/dev/null 2>&1; then
        date -u -d "$1" +%s
    else
        date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s
    fi
}
# Checks that PRIVATE_DIR is a safe directory named by RUN_ID, then creates
# it with mode 700, along with REPORT_DIR.
mkdir_private() {
    [[ $PRIVATE_DIR = /* && $REPORT_DIR = /* &&
        ${PRIVATE_DIR##*/} = "$RUN_ID" && $REPORT_DIR != "$PRIVATE_DIR" &&
        ${PRIVATE_DIR%/*} != / && ! -L $PRIVATE_DIR ]] ||
        fail "Private path must be an absolute, non-symlink directory named by run ID"
    if [[ $COMMAND = down && ! -d $PRIVATE_DIR ]]; then return; fi
    mkdir -p "$PRIVATE_DIR" "$REPORT_DIR"
    chmod 700 "$PRIVATE_DIR"
}
# Stops unless Calico manifest $1 has one calico-node DaemonSet with VXLAN
# Always and IPIP Never.
check_calico_modes() {
    yq eval -o=json 'select(.kind == "DaemonSet" and .metadata.name == "calico-node")' "$1" |
        jq -se '
            def mode($name; $value):
                [.[0].spec.template.spec.containers[]? |
                    select(.name == "calico-node") | .env[]? |
                    select(.name == $name)] as $matches |
                ($matches | length) == 1 and $matches[0].value == $value;
            length == 1 and
            ([.[0].spec.template.spec.containers[]? |
                select(.name == "calico-node")] | length) == 1 and
            mode("CALICO_IPV4POOL_VXLAN"; "Always") and
            mode("CALICO_IPV4POOL_IPIP"; "Never")
        ' >/dev/null ||
        fail "Calico manifest must set CALICO_IPV4POOL_VXLAN=Always and CALICO_IPV4POOL_IPIP=Never for Azure worker Pod traffic"
}
# Checks the env file values, the Azure subscription, the operator address
# and the deadline. Sets the defaults and INFRA_RG, WORKERS_RG and KUBECONFIG.
validate() {
    required RUN_ID PREFIX SUBSCRIPTION LOCATION ZONE DEADLINE OPERATOR_CIDR IMAGE \
        CA_IMAGE WORKLOAD_IMAGE PRIVATE_DIR REPORT_DIR CALICO_MANIFEST CCM_CHART
    [[ $RUN_ID =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && $PREFIX =~ ^[a-z0-9]+$ ]] ||
        fail "Run ID and prefix must be safe resource names"
    (( ${#RUN_ID} <= 63 && ${#PREFIX} <= 8 )) ||
        fail "Run ID must fit a DNS label and prefix must fit worker Node names"
    [[ $DEADLINE =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
        fail "Use an explicit UTC cleanup deadline"
    [[ $SUBSCRIPTION =~ ^[0-9a-fA-F-]{36}$ && $LOCATION = westus2 && $ZONE = 1 ]] ||
        fail "Use a subscription GUID, westus2 and zone 1"
    [[ $OPERATOR_CIDR =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/32$ ]] ||
        fail "Operator CIDR must be one IPv4 /32"
    if [[ $COMMAND != down ]]; then
        [[ $(curl -fsS -4 --max-time 15 https://api.ipify.org)/32 = "$OPERATOR_CIDR" ]] ||
            fail "The operator /32 no longer matches the current public address"
    fi
    [[ $(az account show --query id -o tsv) = "$SUBSCRIPTION" ]] ||
        fail "Azure CLI is on a different subscription"
    if [[ $COMMAND != down ]]; then
        required CALICO_SHA256
        [[ -f $CALICO_MANIFEST && -f $CCM_CHART ]] ||
            fail "Pinned Calico manifest and CCM chart must be supplied before setup"
        [[ $(shasum -a 256 "$CALICO_MANIFEST" | awk '{print $1}') = "$CALICO_SHA256" ]] ||
            fail "Calico manifest does not match the pinned digest"
        check_calico_modes "$CALICO_MANIFEST"
        [[ $(helm show chart "$CCM_CHART" | yq eval '.version' -) = 1.36.0 ]] ||
            fail "CCM chart must be version 1.36.0"
    fi
    mkdir_private
    local now end started
    now=$(date -u +%s)
    end=$(epoch "$DEADLINE")
    if [[ $COMMAND != down ]]; then
        (( end > now )) || fail "Cleanup deadline has passed"
    fi
    if [[ -f $PRIVATE_DIR/started-at ]]; then
        started=$(epoch "$(cat "$PRIVATE_DIR/started-at")")
        (( end - started <= 43200 )) || fail "Cleanup deadline exceeds 12 hours from the first write"
    elif [[ $COMMAND != down ]]; then
        (( end - now <= 43200 )) || fail "Cleanup deadline must be at most 12 hours away"
    fi
    INFRA_RG=$RUN_ID-infra
    WORKERS_RG=$RUN_ID-workers
    SSH_PORT=${SSH_PORT:-2222}
    API_PORT=${API_PORT:-16443}
    VNET_CIDR=${VNET_CIDR:-10.144.0.0/16}
    CP_CIDR=${CP_CIDR:-10.144.0.0/24}
    WORKER_CIDR=${WORKER_CIDR:-10.144.1.0/24}
    BASTION_CIDR=${BASTION_CIDR:-10.144.2.0/26}
    POD_CIDR=${POD_CIDR:-192.168.0.0/16}
    DEMAND_CPU=${DEMAND_CPU:-1200m}
    LARGE_CPU=${LARGE_CPU:-600m}
    RUN_BUDGET_USD=${RUN_BUDGET_USD:-14}
    CLEANUP_RESERVE_USD=${CLEANUP_RESERVE_USD:-3}
    STOP_NEW_HOURS=${STOP_NEW_HOURS:-8}
    [[ $STOP_NEW_HOURS =~ ^[0-9]+$ && $SSH_PORT =~ ^[0-9]+$ &&
        $API_PORT =~ ^[0-9]+$ ]] ||
        fail "Stop time and tunnel ports must be whole numbers"
    local amount='^[0-9]+([.][0-9]+)?$'
    [[ $RUN_BUDGET_USD =~ $amount && $CLEANUP_RESERVE_USD =~ $amount &&
        ${PAST_RUN_COST_USD:-0} =~ $amount ]] ||
        fail "Budget values must be USD amounts such as 14 or 2.50"
    awk -v budget="$RUN_BUDGET_USD" -v past="${PAST_RUN_COST_USD:-0}" \
        -v reserve="$CLEANUP_RESERVE_USD" -v hours="$STOP_NEW_HOURS" \
        'BEGIN { if (budget <= reserve || past < 0 ||
                   budget + past > 30 || hours <= 0 || hours >= 10) exit 1 }' ||
        fail "Run shares must fit the USD 30 combined cap and leave cleanup time"
    KUBECONFIG=$PRIVATE_DIR/kubeconfig
    export RUN_ID PREFIX SUBSCRIPTION LOCATION ZONE DEADLINE OPERATOR_CIDR \
        INFRA_RG WORKERS_RG SSH_PORT API_PORT VNET_CIDR CP_CIDR WORKER_CIDR \
        BASTION_CIDR POD_CIDR DEMAND_CPU LARGE_CPU PRIVATE_DIR REPORT_DIR IMAGE \
        CA_IMAGE WORKLOAD_IMAGE KUBECONFIG RUN_BUDGET_USD CLEANUP_RESERVE_USD STOP_NEW_HOURS
}
# Records the run start time. Stops if the run's groups exist without it.
start_clock() {
    if [[ ! -f $PRIVATE_DIR/started-at ]]; then
        [[ $(az group exists -n "$INFRA_RG") = false &&
            $(az group exists -n "$WORKERS_RG") = false ]] ||
            fail "Do not adopt an existing group without this run's start record"
        date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PRIVATE_DIR/started-at"
    fi
}
# Stops unless resource group $1 has this run's ID and deadline tags.
owned_group() {
    [[ $(az group show -n "$1" --query tags.runID -o tsv) = "$RUN_ID" &&
        $(az group show -n "$1" --query tags.cleanupDeadlineUTC -o tsv) = "$DEADLINE" ]] ||
        fail "Group $1 does not belong to this run and deadline"
}
# Creates resource group $1 if needed and checks that the run owns it.
ensure_group() {
    if [[ $(az group exists -n "$1") = false ]]; then
        az group create -n "$1" -l "$LOCATION" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned_group "$1"
}
# Prints how many resources of type $2 in group $1 have name $3, ignoring
# case. Returns non-zero when Azure cannot list them.
resource_count() {
    local group=$1 type=$2 name=$3
    az resource list -g "$group" --resource-type "$type" -o json |
        jq --arg name "$name" '[.[] | select((.name|ascii_downcase) == ($name|ascii_downcase))] | length'
}
# Stops unless resource ID $1 has this run's ID and deadline tags.
owned() {
    local id=$1
    az resource show --ids "$id" -o json |
        jq -e --arg run "$RUN_ID" --arg deadline "$DEADLINE" \
            '.tags.runID == $run and .tags.cleanupDeadlineUTC == $deadline' >/dev/null ||
        fail "Resource $id is not tagged for this run and deadline"
}
# Prints the ID of resource $3 of provider $2 in group $1.
resource_id() {
    printf '/subscriptions/%s/resourceGroups/%s/providers/%s/%s\n' \
        "$SUBSCRIPTION" "$1" "$2" "$3"
}
# ensure_nsg, ensure_ip and ensure_identity create the run-owned resource
# named $1 in the infra group if needed.
ensure_nsg() {
    local name=$1 id
    id=$(resource_id "$INFRA_RG" Microsoft.Network "networkSecurityGroups/$name")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/networkSecurityGroups "$name") = 0 ]]; then
        az network nsg create -g "$INFRA_RG" -n "$name" -l "$LOCATION" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
}
ensure_ip() {
    local name=$1 id
    id=$(resource_id "$INFRA_RG" Microsoft.Network "publicIPAddresses/$name")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/publicIPAddresses "$name") = 0 ]]; then
        az network public-ip create -g "$INFRA_RG" -n "$name" -l "$LOCATION" \
            --sku Standard --allocation-method Static --zone "$ZONE" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
}
ensure_identity() {
    local name=$1 id
    id=$(resource_id "$INFRA_RG" Microsoft.ManagedIdentity "userAssignedIdentities/$name")
    if [[ $(resource_count "$INFRA_RG" Microsoft.ManagedIdentity/userAssignedIdentities "$name") = 0 ]]; then
        az identity create -g "$INFRA_RG" -n "$name" -l "$LOCATION" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
}
# Reads NSG rules as JSON on stdin. Succeeds when the operator rule allows only
# TCP 22 and 6443 from OPERATOR_CIDR, and every other inbound Allow rule is
# from the VNet or has the POLICY_NAME_PREFIX name prefix.
allowed_nsg_rules() {
    jq -e --arg cidr "$OPERATOR_CIDR" --arg policy "${POLICY_NAME_PREFIX:-}" '
        ([.[] | select(.name == "operator-ssh-api")] | length == 1) and
        (all(.[]; if .name == "operator-ssh-api" then
            .direction == "Inbound" and .access == "Allow" and
            .protocol == "Tcp" and .priority == 100 and
            .sourceAddressPrefix == $cidr and
            ((.sourceAddressPrefixes // []) | length == 0) and
            .destinationPortRange == null and
            ((.destinationPortRanges // []) | sort == ["22", "6443"])
        else true end)) and
        (all(.[]; if .direction == "Inbound" and .access == "Allow" then
            .name == "operator-ssh-api" or
            ($policy != "" and (.name | startswith($policy))) or
            .sourceAddressPrefix == "VirtualNetwork"
        else true end))
    ' >/dev/null
}
# Checks the control-plane NSG rules and records the policy rules in
# policy-nsg-rules.json in REPORT_DIR when they change.
verify_operator_nsg() {
    local rules current=$PRIVATE_DIR/policy-nsg-rules-current-$$.json
    local published=$REPORT_DIR/.policy-nsg-rules-$$.tmp
    owned "$(resource_id "$INFRA_RG" Microsoft.Network "networkSecurityGroups/$PREFIX-cp-nsg")"
    rules=$(az network nsg rule list -g "$INFRA_RG" --nsg-name "$PREFIX-cp-nsg" -o json)
    allowed_nsg_rules <<< "$rules" ||
        fail "Control-plane NSG has a changed /32 rule or an unapproved external inbound Allow"
    jq --arg policy "${POLICY_NAME_PREFIX:-}" '[.[] |
        select($policy != "" and (.name | startswith($policy))) |
        {name,priority,direction,access,protocol,sourceAddressPrefix,
         sourceAddressPrefixes,destinationPortRange,destinationPortRanges}] |
        sort_by(.name)' <<< "$rules" > "$current"
    if [[ ! -f $REPORT_DIR/policy-nsg-rules.json ]] ||
        ! cmp -s "$current" "$REPORT_DIR/policy-nsg-rules.json"; then
        cp "$current" "$published"
        mv -f "$published" "$REPORT_DIR/policy-nsg-rules.json"
        note "Organization policy NSG rules found:"
        jq -c '.[]' "$current"
    fi
    rm -f "$current"
}
# Gives principal $1 the Contributor role at scope $2 if needed, and appends
# the assignment ID, scope and identity name $3 to roles.tmp.
role() {
    local principal=$1 scope=$2 identity=$3 assignment id
    assignment=$(az role assignment list --scope "$scope" -o json |
        jq --arg principal "$principal" --arg scope "$scope" \
        '[.[] | select(.principalId==$principal and
            (.scope|ascii_downcase)==($scope|ascii_downcase) and
            .roleDefinitionName=="Contributor")]')
    if [[ $(jq 'length' <<< "$assignment") = 0 ]]; then
        assignment=$(az role assignment create --assignee-object-id "$principal" \
            --assignee-principal-type ServicePrincipal --role Contributor \
            --scope "$scope" -o json)
        id=$(jq -r '.id' <<< "$assignment")
    else
        [[ $(jq 'length' <<< "$assignment") = 1 ]] || fail "Multiple Contributor assignments at $scope"
        id=$(jq -r '.[0].id' <<< "$assignment")
    fi
    printf '%s %s %s\n' "$id" "$scope" "$identity" >> "$PRIVATE_DIR/roles.tmp"
}
# Creates or checks the VNet, subnets, NSGs, public IPs, NAT gateway and
# route table of the run.
ensure_network() {
    local vnet=$PREFIX-vnet id
    id=$(resource_id "$INFRA_RG" Microsoft.Network "virtualNetworks/$vnet")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/virtualNetworks "$vnet") = 0 ]]; then
        az network vnet create -g "$INFRA_RG" -n "$vnet" -l "$LOCATION" \
            --address-prefixes "$VNET_CIDR" --subnet-name control-plane \
            --subnet-prefixes "$CP_CIDR" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
    local name cidr
    for name in workers AzureBastionSubnet; do
        cidr=$WORKER_CIDR
        [[ $name = AzureBastionSubnet ]] && cidr=$BASTION_CIDR
        if ! az network vnet subnet list -g "$INFRA_RG" --vnet-name "$vnet" -o json |
            jq -e --arg name "$name" 'any(.[]; .name == $name)' >/dev/null; then
            az network vnet subnet create -g "$INFRA_RG" --vnet-name "$vnet" \
                -n "$name" --address-prefixes "$cidr" --output none
        fi
    done
    ensure_nsg "$PREFIX-cp-nsg"
    ensure_nsg "$PREFIX-worker-nsg"
    if ! az network nsg rule list -g "$INFRA_RG" --nsg-name "$PREFIX-cp-nsg" -o json |
        jq -e 'any(.[]; .name == "operator-ssh-api")' >/dev/null; then
        az network nsg rule create -g "$INFRA_RG" --nsg-name "$PREFIX-cp-nsg" \
            -n operator-ssh-api --priority 100 --protocol Tcp \
            --direction Inbound --access Allow --source-address-prefixes "$OPERATOR_CIDR" \
            --destination-port-ranges 22 6443 --output none
    fi
    verify_operator_nsg
    local pip
    for pip in cp nat bastion; do ensure_ip "$PREFIX-$pip-pip"; done
    id=$(resource_id "$INFRA_RG" Microsoft.Network "natGateways/$PREFIX-nat")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/natGateways "$PREFIX-nat") = 0 ]]; then
        az network nat gateway create -g "$INFRA_RG" -n "$PREFIX-nat" -l "$LOCATION" \
            --public-ip-addresses "$PREFIX-nat-pip" --idle-timeout 10 \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
    id=$(resource_id "$INFRA_RG" Microsoft.Network "routeTables/$PREFIX-cp-routes")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/routeTables "$PREFIX-cp-routes") = 0 ]]; then
        az network route-table create -g "$INFRA_RG" -n "$PREFIX-cp-routes" -l "$LOCATION" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$id"
    local subnet nat route nsg
    nat=$(resource_id "$INFRA_RG" Microsoft.Network "natGateways/$PREFIX-nat")
    route=$(resource_id "$INFRA_RG" Microsoft.Network "routeTables/$PREFIX-cp-routes")
    nsg=$(resource_id "$INFRA_RG" Microsoft.Network "networkSecurityGroups/$PREFIX-cp-nsg")
    subnet=$(az network vnet subnet show -g "$INFRA_RG" --vnet-name "$vnet" \
        -n control-plane -o json)
    if ! jq -e --arg nat "$nat" --arg route "$route" --arg nsg "$nsg" \
        '(.natGateway.id//""|ascii_downcase)==($nat|ascii_downcase) and
         (.routeTable.id//""|ascii_downcase)==($route|ascii_downcase) and
         (.networkSecurityGroup.id//""|ascii_downcase)==($nsg|ascii_downcase)' \
        <<< "$subnet" >/dev/null; then
        az network vnet subnet update -g "$INFRA_RG" --vnet-name "$vnet" -n control-plane \
            --nat-gateway "$PREFIX-nat" --route-table "$PREFIX-cp-routes" \
            --network-security-group "$PREFIX-cp-nsg" --output none
    fi
    nsg=$(resource_id "$INFRA_RG" Microsoft.Network "networkSecurityGroups/$PREFIX-worker-nsg")
    subnet=$(az network vnet subnet show -g "$INFRA_RG" --vnet-name "$vnet" -n workers -o json)
    if ! jq -e --arg nat "$nat" --arg nsg "$nsg" \
        '(.natGateway.id//""|ascii_downcase)==($nat|ascii_downcase) and
         (.networkSecurityGroup.id//""|ascii_downcase)==($nsg|ascii_downcase)' \
        <<< "$subnet" >/dev/null; then
        az network vnet subnet update -g "$INFRA_RG" --vnet-name "$vnet" -n workers \
            --nat-gateway "$PREFIX-nat" --network-security-group "$PREFIX-worker-nsg" --output none
    fi
    [[ $(az network vnet subnet show -g "$INFRA_RG" --vnet-name "$vnet" -n control-plane \
        --query routeTable.id -o tsv | tr '[:upper:]' '[:lower:]') = \
        "$(resource_id "$INFRA_RG" Microsoft.Network "routeTables/$PREFIX-cp-routes" | tr '[:upper:]' '[:lower:]')" ]] ||
        fail "CCM route table is missing from the control-plane subnet"
}
# Tags untagged resources in the run's groups with the run ID and deadline.
# Stops on a resource that another run owns or that has an unknown name.
tag_created_resources() {
    local group exists resources name type id run
    for group in "$INFRA_RG" "$WORKERS_RG"; do
        exists=$(az group exists -n "$group") || fail "Cannot check resource group $group"
        [[ $exists = true ]] || continue
        owned_group "$group"
        resources=$(az resource list -g "$group" -o json)
        while IFS=$'\t' read -r name type id run; do
            [[ -n $name ]] || continue
            if [[ $type = Microsoft.Compute/virtualMachines/extensions ||
                $type = Microsoft.Compute/virtualMachineScaleSets/extensions ]]; then
                [[ $id = *"/$PREFIX-cp/extensions/"* || $id = *"/$PREFIX-"*"/extensions/"* ]] ||
                    fail "An extension is not under this run's VM"
                continue
            fi
            [[ $name = "$PREFIX"-* ||
                ( -n ${POLICY_NAME_PREFIX:-} && $name = "$POLICY_NAME_PREFIX"* ) ]] ||
                fail "Unknown resource $name is in the run-owned group"
            if [[ $run != "$RUN_ID" ]]; then
                [[ $run = null ]] || fail "Resource $id belongs to another run"
                az resource tag --ids "$id" --is-incremental \
                    --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
            fi
            owned "$id"
        done < <(jq -r '.[]|[.name,.type,.id,(.tags.runID//"null")]|@tsv' <<< "$resources")
    done
}
# Creates the autoscaler and CCM identities and their role assignments, and
# saves the assignment IDs in the roles file.
ensure_roles() {
    ensure_identity "$PREFIX-ca"
    ensure_identity "$PREFIX-ccm"
    local ca ccm w i
    ca=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ca" --query principalId -o tsv)
    ccm=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ccm" --query principalId -o tsv)
    w=/subscriptions/$SUBSCRIPTION/resourceGroups/$WORKERS_RG
    i=/subscriptions/$SUBSCRIPTION/resourceGroups/$INFRA_RG
    : > "$PRIVATE_DIR/roles.tmp"
    role "$ca" "$w" "$PREFIX-ca"
    role "$ccm" "$w" "$PREFIX-ccm"
    role "$ccm" "$i" "$PREFIX-ccm"
    mv "$PRIVATE_DIR/roles.tmp" "$PRIVATE_DIR/roles"
}
# Creates the run's SSH key pair, or checks that the saved pair matches.
ensure_ssh_key() {
    if [[ -f $PRIVATE_DIR/id_ed25519 || -f $PRIVATE_DIR/id_ed25519.pub ]]; then
        [[ -f $PRIVATE_DIR/id_ed25519 && -f $PRIVATE_DIR/id_ed25519.pub ]] ||
            fail "SSH key pair is incomplete; do not replace a key used by this run"
        ssh-keygen -y -f "$PRIVATE_DIR/id_ed25519" |
            awk '{print $1, $2}' |
            diff - <(awk '{print $1, $2}' "$PRIVATE_DIR/id_ed25519.pub") >/dev/null ||
            fail "Run-specific SSH private and public keys do not match"
    else
        ssh-keygen -q -t ed25519 -N '' -f "$PRIVATE_DIR/id_ed25519"
    fi
    chmod 600 "$PRIVATE_DIR/id_ed25519"
}
# Creates or checks the control-plane NIC, VM and OS disk, and saves the
# VM's private and public addresses.
ensure_cp() {
    local nic vm ccm ca private_ip public_ip
    nic=$(resource_id "$INFRA_RG" Microsoft.Network "networkInterfaces/$PREFIX-cp-nic")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/networkInterfaces "$PREFIX-cp-nic") = 0 ]]; then
        az network nic create -g "$INFRA_RG" -n "$PREFIX-cp-nic" -l "$LOCATION" \
            --vnet-name "$PREFIX-vnet" --subnet control-plane --ip-forwarding true \
            --public-ip-address "$PREFIX-cp-pip" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --output none
    fi
    owned "$nic"
    az network nic show -g "$INFRA_RG" -n "$PREFIX-cp-nic" -o json |
        jq -e '(.enableIpForwarding == true or .enableIPForwarding == true)' >/dev/null ||
        fail "The control-plane NIC must have IP forwarding enabled"
    vm=$(resource_id "$INFRA_RG" Microsoft.Compute "virtualMachines/$PREFIX-cp")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Compute/virtualMachines "$PREFIX-cp") = 0 ]]; then
        [[ -f $PRIVATE_DIR/id_ed25519.pub ]] || fail "The private SSH key is missing"
        ccm=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ccm" --query id -o tsv)
        ca=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ca" --query id -o tsv)
        jq -n --arg location "$LOCATION" --arg zone "$ZONE" --arg run "$RUN_ID" \
            --arg deadline "$DEADLINE" --arg image "$IMAGE" --arg prefix "$PREFIX" \
            --arg ccm "$ccm" --arg ca "$ca" --arg nic "$nic" \
            --rawfile ssh "$PRIVATE_DIR/id_ed25519.pub" \
            '{location:$location,zones:[$zone],
              tags:{runID:$run,cleanupDeadlineUTC:$deadline,"autoscaler-e2e-run":$run},
              identity:{type:"UserAssigned",userAssignedIdentities:{($ccm):{},($ca):{}}},
              properties:{hardwareProfile:{vmSize:"Standard_D2s_v5"},
                storageProfile:{imageReference:{communityGalleryImageId:$image},
                  osDisk:{createOption:"FromImage",diskSizeGB:128,
                    managedDisk:{storageAccountType:"StandardSSD_LRS"},deleteOption:"Delete"}},
                osProfile:{computerName:($prefix+"-cp"),adminUsername:"azureuser",
                  linuxConfiguration:{disablePasswordAuthentication:true,
                    ssh:{publicKeys:[{path:"/home/azureuser/.ssh/authorized_keys",keyData:($ssh|rtrimstr("\n"))}]}}},
                networkProfile:{networkInterfaces:[{id:$nic,properties:{primary:true,deleteOption:"Delete"}}]}}}' \
            > "$PRIVATE_DIR/cp-vm.json"
        az rest --method put --url "https://management.azure.com${vm}?api-version=2024-11-01" \
            --headers Content-Type=application/json --body "@$PRIVATE_DIR/cp-vm.json" --output none
    fi
    owned "$vm"
    az vm wait -g "$INFRA_RG" -n "$PREFIX-cp" --created --interval 10 --timeout 900
    [[ $(az vm show -g "$INFRA_RG" -n "$PREFIX-cp" --query hardwareProfile.vmSize -o tsv) = Standard_D2s_v5 ]] ||
        fail "Unexpected control-plane VM size"
    private_ip=$(az network nic show -g "$INFRA_RG" -n "$PREFIX-cp-nic" \
        --query 'ipConfigurations[0].privateIPAddress' -o tsv)
    public_ip=$(az network public-ip show -g "$INFRA_RG" -n "$PREFIX-cp-pip" --query ipAddress -o tsv)
    [[ -n $private_ip && -n $public_ip ]] || fail "Control-plane addresses are missing"
    printf '%s\n' "$private_ip" > "$PRIVATE_DIR/cp-private-ip"
    printf '%s\n' "$public_ip" > "$PRIVATE_DIR/cp-public-ip"
    local disk
    disk=$(az vm show -g "$INFRA_RG" -n "$PREFIX-cp" --query storageProfile.osDisk.managedDisk.id -o tsv)
    [[ -n $disk ]] || fail "The control-plane OS disk was not created"
    if ! az resource show --ids "$disk" -o json |
        jq -e --arg run "$RUN_ID" --arg deadline "$DEADLINE" \
            '.tags.runID == $run and .tags.cleanupDeadlineUTC == $deadline' >/dev/null; then
        az disk update --ids "$disk" \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" \
                autoscaler-e2e-run="$RUN_ID" --output none
    fi
    owned "$disk"
}
# Creates the Bastion host if needed and waits until tunneling is ready.
ensure_bastion() {
    local id
    id=$(resource_id "$INFRA_RG" Microsoft.Network "bastionHosts/$PREFIX-bastion")
    if [[ $(resource_count "$INFRA_RG" Microsoft.Network/bastionHosts "$PREFIX-bastion") = 0 ]]; then
        az network bastion create --name "$PREFIX-bastion" -g "$INFRA_RG" \
            --vnet-name "$PREFIX-vnet" --public-ip-address "$PREFIX-bastion-pip" \
            --sku Standard --enable-tunneling true \
            --tags runID="$RUN_ID" cleanupDeadlineUTC="$DEADLINE" --no-wait --output none
    fi
    owned "$id"
    local state i
    for ((i=0; i<90; i++)); do
        state=$(az network bastion show -g "$INFRA_RG" -n "$PREFIX-bastion" \
            --query provisioningState -o tsv)
        [[ $state = Succeeded ]] && break
        [[ $state = Updating || $state = Creating ]] ||
            fail "Bastion provisioning stopped at $state"
        sleep 10
    done
    [[ $state = Succeeded &&
        $(az network bastion show -g "$INFRA_RG" -n "$PREFIX-bastion" \
            --query enableTunneling -o tsv) = true ]] ||
        fail "Bastion tunneling did not become ready"
}
# Replaces the script with a Bastion tunnel to the control plane. $1 is ssh
# or api.
tunnel() {
    local kind=$1 port resource pidfile
    case $kind in
        ssh) port=$SSH_PORT; resource=22 ;;
        api) port=$API_PORT; resource=6443 ;;
        *) fail "Use tunnel ssh or tunnel api" ;;
    esac
    owned_group "$INFRA_RG"
    [[ $(az network bastion show -g "$INFRA_RG" -n "$PREFIX-bastion" \
        --query provisioningState -o tsv) = Succeeded ]] || fail "Bastion is not ready"
    pidfile=$PRIVATE_DIR/tunnel-$kind.pid
    if [[ -f $pidfile ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        fail "The $kind tunnel is already running"
    fi
    local listeners
    listeners=$(tunnel_listeners "$port")
    [[ -z $listeners ]] || fail "Local tunnel port $port is occupied"
    printf '%s\n' "$$" > "$pidfile"
    note "Starting the owned $kind Bastion tunnel on local port $port"
    exec az network bastion tunnel --name "$PREFIX-bastion" -g "$INFRA_RG" \
        --target-resource-id "$(resource_id "$INFRA_RG" Microsoft.Compute "virtualMachines/$PREFIX-cp")" \
        --resource-port "$resource" --port "$port"
}
# Runs a command on the control plane through the SSH tunnel.
ssh_cp() {
    ssh -p "$SSH_PORT" -i "$PRIVATE_DIR/id_ed25519" \
        -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$PRIVATE_DIR/known_hosts" \
        -o ConnectTimeout=20 -o ServerAliveInterval=30 azureuser@127.0.0.1 "$@"
}
# Copies local file $1 to path $2 on the control plane.
copy_cp() {
    scp -q -P "$SSH_PORT" -i "$PRIVATE_DIR/id_ed25519" \
        -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$PRIVATE_DIR/known_hosts" \
        "$1" "azureuser@127.0.0.1:$2"
}
# Copies the admin kubeconfig through the tunnel if needed. Stops unless it
# reaches the cluster with the saved kube-system UID.
ensure_kubeconfig() {
    if [[ ! -f $KUBECONFIG ]]; then
        local copied=$KUBECONFIG.tmp
        if ! ssh_cp 'sudo install -m 600 -o azureuser /etc/kubernetes/admin.conf /home/azureuser/admin.conf' ||
            ! scp -q -P "$SSH_PORT" -i "$PRIVATE_DIR/id_ed25519" \
                -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$PRIVATE_DIR/known_hosts" \
                azureuser@127.0.0.1:/home/azureuser/admin.conf "$copied" ||
            ! yq -i '(.clusters[0].cluster.server) = "https://127.0.0.1:" + strenv(API_PORT) |
                (.contexts[0].name) = strenv(PREFIX) | (.current-context) = strenv(PREFIX)' "$copied"; then
            rm -f "$copied"
            fail "Cannot copy the control-plane kubeconfig"
        fi
        chmod 600 "$copied"
        mv -f "$copied" "$KUBECONFIG"
    fi
    [[ $(kubectl --kubeconfig "$KUBECONFIG" --request-timeout=30s get namespace kube-system \
        -o jsonpath='{.metadata.uid}') = "$(cat "$PRIVATE_DIR/cluster-uid")" ]] ||
        fail "Kubeconfig points to another cluster"
}
# Runs kubeadm init once, then installs Calico, the CCM and the candidate
# image on the control plane.
ensure_cluster() {
    [[ -f $CALICO_MANIFEST && -f $CCM_CHART ]] || fail "Pinned Calico manifest and CCM chart are required"
    required CALICO_SHA256
    [[ $(shasum -a 256 "$CALICO_MANIFEST" | awk '{print $1}') = "$CALICO_SHA256" ]] ||
        fail "Calico manifest does not match the pinned SHA-256"
    check_calico_modes "$CALICO_MANIFEST"
    [[ $(helm show chart "$CCM_CHART" | yq eval '.version' -) = 1.36.0 ]] ||
        fail "The CCM chart version must be 1.36.0"
    [[ $(ssh_cp hostname) = "$PREFIX-cp" ]] ||
        fail "The owned Bastion SSH tunnel does not reach this control plane"
    if [[ -f $PRIVATE_DIR/cluster-uid && ! -s $PRIVATE_DIR/cluster-uid ]]; then
        note "Removing an incomplete private cluster fingerprint before retry"
        rm -f "$PRIVATE_DIR/cluster-uid"
    fi
    if [[ ! -f $PRIVATE_DIR/cluster-uid ]]; then
        [[ -f $PRIVATE_DIR/id_ed25519 ]] || fail "The run-owned SSH key is missing"
        [[ -f $PRIVATE_DIR/cp-private-ip && -f $PRIVATE_DIR/cp-public-ip ]] ||
            fail "The control plane is not ready"
        local private_ip public_ip ccm_id
        private_ip=$(cat "$PRIVATE_DIR/cp-private-ip")
        public_ip=$(cat "$PRIVATE_DIR/cp-public-ip")
        ccm_id=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ccm" --query clientId -o tsv)
        cat > "$PRIVATE_DIR/kubeadm-init.yaml" <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: $private_ip
  bindPort: 6443
nodeRegistration:
  name: $PREFIX-cp
  kubeletExtraArgs:
  - name: cloud-provider
    value: external
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: $PREFIX
kubernetesVersion: v1.37.0
controlPlaneEndpoint: $private_ip:6443
apiServer:
  certSANs:
  - $private_ip
  - $public_ip
  - 127.0.0.1
  - localhost
controllerManager:
  extraArgs:
  - name: cloud-provider
    value: external
networking:
  dnsDomain: cluster.local
  podSubnet: $POD_CIDR
  serviceSubnet: 10.96.0.0/12
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
EOF
        local tenant
        tenant=$(az account show --query tenantId -o tsv)
        [[ -n $tenant ]] || fail "Azure CLI returned no tenant ID"
        jq -n --arg subscription "$SUBSCRIPTION" --arg tenant "$tenant" --arg workers "$WORKERS_RG" \
            --arg location "$LOCATION" --arg subnet workers --arg nsg "$PREFIX-worker-nsg" \
            --arg vnet "$PREFIX-vnet" --arg infra "$INFRA_RG" --arg identity "$ccm_id" \
            '{cloud:"AzurePublicCloud",tenantId:$tenant,subscriptionId:$subscription,
              resourceGroup:$workers,location:$location,vmType:"vmss",subnetName:$subnet,
              securityGroupName:$nsg,vnetName:$vnet,vnetResourceGroup:$infra,
              useManagedIdentityExtension:true,userAssignedIdentityID:$identity,
              useInstanceMetadata:true,loadBalancerSku:"standard",
              excludeMasterFromStandardLB:true,cloudProviderBackoff:true,
              cloudProviderRatelimit:true}' > "$PRIVATE_DIR/azure.json"
        copy_cp "$PRIVATE_DIR/kubeadm-init.yaml" /home/azureuser/kubeadm-init.yaml
        copy_cp "$PRIVATE_DIR/azure.json" /home/azureuser/azure.json
        ssh_cp 'sudo mkdir -p /etc/kubernetes && sudo install -m 600 /home/azureuser/azure.json /etc/kubernetes/azure.json'
        if ! ssh_cp 'sudo test -f /etc/kubernetes/admin.conf'; then
            ssh_cp 'sudo kubeadm init --config /home/azureuser/kubeadm-init.yaml >/dev/null 2>&1' ||
                fail "kubeadm init failed; repair only this owned VM, then rerun cluster"
        fi
        local uid
        uid=$(ssh_cp 'sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get namespace kube-system -o jsonpath="{.metadata.uid}"') ||
            fail "Control-plane API is not ready; inspect only this run's kubelet"
        [[ $uid =~ ^[a-f0-9-]{36}$ ]] || fail "Kubernetes returned an invalid cluster fingerprint"
        printf '%s\n' "$uid" > "$PRIVATE_DIR/cluster-uid"
    fi
    ensure_kubeconfig
    kubectl --kubeconfig "$KUBECONFIG" label node "$PREFIX-cp" \
        "kubernetes.azure.com/resource-group=$INFRA_RG" --overwrite >/dev/null
    kubectl --kubeconfig "$KUBECONFIG" apply -f "$CALICO_MANIFEST" >/dev/null
    cat > "$PRIVATE_DIR/ccm-values.yaml" <<EOF
cloudControllerManager:
  allocateNodeCidrs: "false"
  clusterCIDR: "$POD_CIDR"
  configureCloudRoutes: "false"
  federatedTokenPath: ""
  imageName: azure-cloud-controller-manager
  imagePullPolicy: IfNotPresent
  imageRepository: mcr.microsoft.com/oss/v2/kubernetes
  imageTag: "v1.36.6@sha256:50a0f9bd1bd836e7e52f70563a1ec02d6db27154744ef5f00adac30dd735c655"
  replicas: 1
cloudNodeManager:
  enableHealthProbeProxy: false
  imageName: azure-cloud-node-manager
  imagePullPolicy: IfNotPresent
  imageRepository: mcr.microsoft.com/oss/v2/kubernetes
  imageTag: "v1.36.6@sha256:fcb63aa3a7cdfded1ee8dfd092d2f1ec518d4f62e4ce4f7af369058084913cea"
  useInstanceMetadata: "true"
  waitRoutes: "false"
EOF
    helm template "$PREFIX-ccm" "$CCM_CHART" -n kube-system \
        -f "$PRIVATE_DIR/ccm-values.yaml" > "$PRIVATE_DIR/ccm-rendered.yaml"
    kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/ccm-rendered.yaml" >/dev/null
    local deploy
    for deploy in coredns calico-kube-controllers; do
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system patch deployment "$deploy" \
            --type merge -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$PREFIX-cp\"},\"tolerations\":[{\"key\":\"node-role.kubernetes.io/control-plane\",\"operator\":\"Exists\",\"effect\":\"NoSchedule\"},{\"key\":\"node.cloudprovider.kubernetes.io/uninitialized\",\"operator\":\"Exists\",\"effect\":\"NoSchedule\"}]}}}}" >/dev/null
    done
    local obj
    for obj in daemonset/calico-node daemonset/cloud-node-manager \
        deployment/cloud-controller-manager deployment/calico-kube-controllers deployment/coredns; do
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status "$obj" --timeout=7m >/dev/null
    done
    [[ $(kubectl --kubeconfig "$KUBECONFIG" get node "$PREFIX-cp" \
        -o json | jq -r '[.status.conditions[]|select(.type=="Ready")|.status][0]') = True ]] ||
        fail "The control plane did not become Ready"
    [[ -f $PRIVATE_DIR/ca-amd64.tar ]] || fail "Run image before cluster to create the private candidate archive"
    copy_cp "$PRIVATE_DIR/ca-amd64.tar" /home/azureuser/ca-amd64.tar
    ssh_cp 'sudo ctr -n k8s.io images import /home/azureuser/ca-amd64.tar >/dev/null && rm /home/azureuser/ca-amd64.tar'
    ssh_cp 'sudo ctr -n k8s.io images list' | grep -F "$CA_IMAGE" >/dev/null ||
        fail "The exact CA image is not loaded on the control plane"
}
# Writes the cloud-init file for pool label $1, with optional taint value $2.
# Creates and saves one kubeadm join command for the run.
join_config() {
    local label=$1 taint=${2:-} endpoint token hash
    if [[ ! -f $PRIVATE_DIR/join-command ]]; then
        if ! ssh_cp 'sudo kubeadm token create --ttl 10h --print-join-command' \
            > "$PRIVATE_DIR/join-command.tmp"; then
            rm -f "$PRIVATE_DIR/join-command.tmp"
            fail "Cannot create a kubeadm join command"
        fi
        mv -f "$PRIVATE_DIR/join-command.tmp" "$PRIVATE_DIR/join-command"
    fi
    local cmd action token_flag hash_flag
    read -r cmd action endpoint token_flag token hash_flag hash < "$PRIVATE_DIR/join-command"
    [[ $cmd = kubeadm && $action = join &&
        $endpoint = "$(cat "$PRIVATE_DIR/cp-private-ip"):6443" &&
        $token_flag = --token && $hash_flag = --discovery-token-ca-cert-hash &&
        $token =~ ^[a-z0-9]{6}\.[a-z0-9]{16}$ &&
        $hash =~ ^sha256:[a-f0-9]{64}$ ]] ||
        fail "Private kubeadm join command does not match this control plane"
    local taint_arg=
    if [[ -n $taint ]]; then
        taint_arg="      - name: register-with-taints
        value: autoscaler-e2e-run=$taint"
    fi
    cat > "$PRIVATE_DIR/join-$label.yaml" <<EOF
#cloud-config
write_files:
- path: /etc/kubernetes/kubeadm-join.yaml
  owner: root:root
  permissions: '0600'
  content: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: JoinConfiguration
    discovery:
      bootstrapToken:
        apiServerEndpoint: $endpoint
        token: $token
        caCertHashes:
        - $hash
    nodeRegistration:
      kubeletExtraArgs:
      - name: cloud-provider
        value: external
      - name: node-labels
        value: acceptance-pool=$label
$taint_arg
runcmd:
- [bash, -c, "kubeadm join --config /etc/kubernetes/kubeadm-join.yaml > /var/log/kubeadm-join.log 2>&1"]
EOF
}
# Writes the VMSS model for pool $1 to model-$1.json. Azure redacts
# customData on reads, so later steps reuse this file.
vmss_model() {
    local name=$1 label=$2 min=$3 max=$4 size=$5 image=$6 bootstrap=$7 special=${8:-normal}
    local cloudinit=$PRIVATE_DIR/join-$label.yaml
    if [[ $bootstrap = join ]]; then
        join_config "$label" "${POOL_TAINT:-}"
    else
        : > "$PRIVATE_DIR/join-$label.yaml"
    fi
    [[ -f $PRIVATE_DIR/id_ed25519.pub ]] || fail "Run the infra step before creating a pool"
    local subnet
    subnet=$(resource_id "$INFRA_RG" Microsoft.Network "virtualNetworks/$PREFIX-vnet/subnets/workers")
    jq -n --arg location "$LOCATION" --arg zone "$ZONE" --arg run "$RUN_ID" \
        --arg deadline "$DEADLINE" --arg name "$name" --arg label "$label" \
        --arg min "$min" --arg max "$max" --arg size "$size" --arg image "$image" \
        --arg subnet "$subnet" --arg bootstrap "$bootstrap" --arg special "$special" \
        --arg taint "${POOL_TAINT:-}" --rawfile ssh "$PRIVATE_DIR/id_ed25519.pub" \
        --rawfile cloudinit "$cloudinit" \
        '{location:$location,zones:[$zone],sku:{name:$size,tier:"Standard",capacity:0},
          tags:{runID:$run,cleanupDeadlineUTC:$deadline,"autoscaler-e2e-run":$run,
            "cluster-autoscaler-name":$run,min:$min,max:$max,
            "acceptance-pool":$label,
            "k8s.io_cluster-autoscaler_node-template_label_acceptance-pool":$label},
          properties:{orchestrationMode:"Uniform",overprovision:false,singlePlacementGroup:true,
            upgradePolicy:{mode:"Manual"},
            virtualMachineProfile:{
              storageProfile:{imageReference:($image|if startswith("/CommunityGalleries/") then
                {communityGalleryImageId:.} else
                ($image|split(":")|{publisher:.[0],offer:.[1],sku:.[2],version:.[3]}) end),
                osDisk:{createOption:"FromImage",diskSizeGB:128,
                  managedDisk:{storageAccountType:"StandardSSD_LRS"}}},
              osProfile:{computerNamePrefix:($name|gsub("-";"")),adminUsername:"azureuser",
                linuxConfiguration:{disablePasswordAuthentication:true,
                  ssh:{publicKeys:[{path:"/home/azureuser/.ssh/authorized_keys",keyData:($ssh|rtrimstr("\n"))}]}}},
              networkProfile:{networkInterfaceConfigurations:[{
                name:($name+"-nic"),properties:{primary:true,enableIPForwarding:true,
                  ipConfigurations:[{name:"ipconfig1",properties:{
                    subnet:{id:$subnet},privateIPAddressVersion:"IPv4"}}]}}]}}}}
         | if $bootstrap=="join" then
             .properties.virtualMachineProfile.osProfile.customData=($cloudinit|@base64)
           else . end
         | if $taint != "" then
             .tags["k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run"]=$taint
           else . end
         | if $special=="no-join" then
             .tags["autoscaler-e2e-no-join"]=$run
           elif $special=="cse" then
             .tags["autoscaler-e2e-failing-extension"]=$run |
             .properties.virtualMachineProfile.extensionProfile={extensions:[{
               name:"CustomScript",properties:{
                 publisher:"Microsoft.Azure.Extensions",type:"CustomScript",
                 typeHandlerVersion:"2.1",autoUpgradeMinorVersion:false,
                 suppressFailures:false,
                 settings:{commandToExecute:"/bin/sh -c \u0027exit 42\u0027"}}}]}
           elif $special=="spot" then
             .properties.virtualMachineProfile.priority="Spot" |
             .properties.virtualMachineProfile.evictionPolicy="Delete" |
             .properties.virtualMachineProfile.billingProfile={maxPrice:-1} |
             .properties.spotRestorePolicy={enabled:false}
           else . end' > "$PRIVATE_DIR/model-$name.json"
    [[ $(jq -r '.tags.runID' "$PRIVATE_DIR/model-$name.json") = "$RUN_ID" ]] ||
        fail "Pool model lost its run tag"
}
# pool_capacity, pool_actual and pool_nics print the capacity, VM count and
# NIC count of VMSS $1.
pool_capacity() {
    az vmss show -g "$WORKERS_RG" -n "$1" --query sku.capacity -o tsv
}
pool_actual() {
    az vmss list-instances -g "$WORKERS_RG" -n "$1" --query 'length(@)' -o tsv
}
pool_nics() {
    az vmss nic list -g "$WORKERS_RG" --vmss-name "$1" --query 'length(@)' -o tsv
}
# Reports whether VMSS $1 exists. Stops when Azure cannot list scale sets,
# so a failed call never reads as absent.
pool_exists() {
    local count
    count=$(resource_count "$WORKERS_RG" Microsoft.Compute/virtualMachineScaleSets "$1") ||
        fail "Cannot list scale sets in $WORKERS_RG"
    [[ $count = 1 ]]
}
# Stops unless VMSS $1 is run-owned with bounds $2 and $3, SKU $4 and the
# fixture model settings.
check_pool() {
    local name=$1 min=$2 max=$3 size=$4 id model
    id=$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")
    owned "$id"
    model=$(az vmss show -g "$WORKERS_RG" -n "$name" -o json)
    jq -e --arg min "$min" --arg max "$max" --arg size "$size" \
        '.tags.min==$min and .tags.max==$max and .sku.name==$size and
         .overprovision==false and .orchestrationMode=="Uniform" and
         (.zones==["1"]) and
         (.virtualMachineProfile.networkProfile.networkInterfaceConfigurations |
            all(.[]; .enableIPForwarding == true or .enableIpForwarding == true))' \
        <<< "$model" >/dev/null ||
        fail "Pool $name does not have its authorized bounds and model"
}
# Creates VMSS $1 if needed and checks it. Scales it from zero to capacity
# $5 when that is the only difference.
ensure_pool() {
    local name=$1 label=$2 min=$3 max=$4 capacity=$5 size=$6 image=$7 bootstrap=$8 special=${9:-normal}
    [[ $name = "$PREFIX"-* && $min =~ ^[0-9]+$ && $max =~ ^[0-9]+$ &&
        $capacity =~ ^[0-9]+$ ]] || fail "Pool name or capacity is invalid"
    (( capacity <= max )) || fail "Pool capacity is larger than its maximum"
    if ! pool_exists "$name"; then
        vmss_model "$name" "$label" "$min" "$max" "$size" "$image" "$bootstrap" "$special"
        az rest --method put \
            --url "https://management.azure.com$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")?api-version=2024-11-01" \
            --headers Content-Type=application/json \
            --body "@$PRIVATE_DIR/model-$name.json" --output none
        az vmss wait -g "$WORKERS_RG" -n "$name" --created --interval 10 --timeout 900
    fi
    check_pool "$name" "$min" "$max" "$size"
    [[ -f $PRIVATE_DIR/model-$name.json ]] ||
        fail "Pool $name exists but its private model is missing; Azure redacts customData"
    if [[ $(pool_capacity "$name") != "$capacity" ]]; then
        [[ $(pool_capacity "$name") = 0 && $(pool_actual "$name") = 0 &&
            $(pool_nics "$name") = 0 && $capacity = 1 ]] ||
            fail "Pool $name is not at its expected starting capacity"
        az vmss scale -g "$WORKERS_RG" -n "$name" --new-capacity 1 --output none
    fi
    [[ $(pool_capacity "$name") = "$capacity" ]] || fail "Pool $name did not reach $capacity"
}
# Waits until VMSS $1 has $3 VMs and $3 Ready Nodes with pool label $2.
wait_pool_ready() {
    local name=$1 label=$2 expected=$3 i ready
    for ((i=0; i<90; i++)); do
        ready=$(kubectl --kubeconfig "$KUBECONFIG" get nodes \
            -l "acceptance-pool=$label" -o json |
            jq '[.items[]|select(any(.status.conditions[]?;.type=="Ready" and .status=="True"))]|length')
        [[ $ready = "$expected" && $(pool_actual "$name") = "$expected" ]] && return
        sleep 10
    done
    fail "Pool $name did not get $expected Ready worker Nodes"
}
# EXIT trap of network_probe. Deletes probe namespace $1 if the run owns it,
# then exits with the status of the probe.
cleanup_network_probe() {
    local status=$? namespace=$1 existing
    trap - EXIT
    rm -f "$PRIVATE_DIR/network-probe-response"
    existing=$(kubectl --kubeconfig "$KUBECONFIG" get namespace "$namespace" \
        --ignore-not-found -o json) ||
        fail "Cannot check the network probe namespace during cleanup"
    if [[ -n $existing ]]; then
        [[ $(jq -r '.metadata.labels["autoscaler-e2e-run"] // ""' <<< "$existing") = "$RUN_ID" ]] ||
            fail "Refusing to remove a network probe namespace from another run"
        kubectl --kubeconfig "$KUBECONFIG" delete namespace "$namespace" \
            --wait=true --timeout=3m >/dev/null ||
            fail "Could not remove the run-owned network probe namespace"
    fi
    exit "$status"
}
# Runs one HTTP request from Pod $2 to IP $3 and kills it after $4 seconds.
probe_exec() {
    local namespace=$1 source=$2 target_ip=$3 seconds=$4 pid end
    kubectl --kubeconfig "$KUBECONFIG" --request-timeout=8s \
        -n "$namespace" exec "$source" -- wget -q -T 3 -O - \
        "http://$target_ip:8080/health" \
        > "$PRIVATE_DIR/network-probe-response" 2>/dev/null &
    pid=$!
    end=$((SECONDS + seconds))
    while kill -0 "$pid" 2>/dev/null && (( SECONDS < end )); do sleep 1; done
    if kill -0 "$pid" 2>/dev/null; then
        kill -KILL "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
        return 124
    fi
    wait "$pid"
}
# Retries probe_exec from Pod $2 to IP $3 until it gets "ok" or $4 seconds
# pass.
probe_http() {
    local namespace=$1 source=$2 target_ip=$3 response remaining window=${4:-120}
    local end=$((SECONDS + window))
    while (( SECONDS < end )); do
        remaining=$((end - SECONDS))
        if (( remaining > 10 )); then remaining=10; fi
        if probe_exec "$namespace" "$source" "$target_ip" "$remaining"; then
            response=$(<"$PRIVATE_DIR/network-probe-response")
            [[ $response = ok ]] && return 0
        fi
        if (( SECONDS < end )); then sleep 5; fi
    done
    fail "Pod-IP HTTP from $source to $target_ip failed within $window seconds"
}
# Starts two run-owned Pods, on two Ready workers when there are two, and
# checks HTTP between their Pod IPs in both directions.
network_probe() (
    local namespace=azure-e2e-$PREFIX-network workers first second existing pods ip_a ip_b name node
    workers=$(kubectl --kubeconfig "$KUBECONFIG" get nodes -o json |
        jq -r --arg path "/resourcegroups/$WORKERS_RG/providers/microsoft.compute/virtualmachinescalesets/$PREFIX-" '
            [.items[] | select((.spec.providerID // "" | ascii_downcase) |
                contains($path | ascii_downcase)) |
                select(.metadata.labels["acceptance-pool"] != null) |
                select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) |
                .metadata.name] | sort | .[]')
    [[ -n $workers ]] || fail "No Ready run-owned worker Node is available for the Pod-IP network check"
    first=${workers%%$'\n'*}
    if [[ $workers = *$'\n'* ]]; then
        second=${workers#*$'\n'}
        second=${second%%$'\n'*}
    else
        second=$PREFIX-cp
        [[ $(kubectl --kubeconfig "$KUBECONFIG" get node "$second" -o json |
            jq -r '[.status.conditions[] | select(.type == "Ready") | .status][0]') = True ]] ||
            fail "The control plane is not Ready for the Pod-IP network check"
        note "Only one worker is Ready; worker-to-control-plane Pod-IP HTTP does not prove worker-to-worker traffic"
    fi
    trap 'cleanup_network_probe "$namespace"' EXIT
    existing=$(kubectl --kubeconfig "$KUBECONFIG" get namespace "$namespace" \
        --ignore-not-found -o json) ||
        fail "Cannot check the run-owned network probe namespace"
    if [[ -n $existing ]]; then
        [[ $(jq -r '.metadata.labels["autoscaler-e2e-run"] // ""' <<< "$existing") = "$RUN_ID" ]] ||
            fail "Network probe namespace belongs to another run"
    else
        jq -n --arg name "$namespace" --arg run "$RUN_ID" \
            '{apiVersion:"v1",kind:"Namespace",
              metadata:{name:$name,labels:{"autoscaler-e2e-run":$run}}}' |
            kubectl --kubeconfig "$KUBECONFIG" create -f - >/dev/null ||
            fail "Cannot create the run-owned network probe namespace"
    fi
    for name in probe-a probe-b; do
        node=$first
        [[ $name = probe-b ]] && node=$second
        jq -n --arg namespace "$namespace" --arg run "$RUN_ID" \
            --arg name "$name" --arg node "$node" --arg image "$WORKLOAD_IMAGE" '
            {apiVersion:"v1",kind:"Pod",
             metadata:{name:$name,namespace:$namespace,labels:{"autoscaler-e2e-run":$run}},
             spec:{nodeName:$node,restartPolicy:"Always",
               tolerations:[{operator:"Exists"}],
               containers:[{name:"http",image:$image,
                 command:["sh","-c"],
                 args:["printf \"ok\\n\" > /tmp/health; exec httpd -f -p 8080 -h /tmp"],
                 resources:{requests:{cpu:"5m",memory:"16Mi"}}}]}}' |
            kubectl --kubeconfig "$KUBECONFIG" create -f - >/dev/null ||
            fail "Cannot start the network probe Pod $name on $node"
    done
    kubectl --kubeconfig "$KUBECONFIG" -n "$namespace" wait \
        --for=condition=Ready pod/probe-a pod/probe-b --timeout=4m >/dev/null ||
        fail "The network probe Pods did not become Ready on $first and $second"
    pods=$(kubectl --kubeconfig "$KUBECONFIG" -n "$namespace" \
        get pod probe-a probe-b -o json) ||
        fail "Cannot read the network probe Pod IPs"
    jq -e --arg first "$first" --arg second "$second" '
        ([.items[] | select(.metadata.name == "probe-a" and
            .spec.nodeName == $first and .status.podIP != null)] | length) == 1 and
        ([.items[] | select(.metadata.name == "probe-b" and
            .spec.nodeName == $second and .status.podIP != null)] | length) == 1
    ' <<< "$pods" >/dev/null ||
        fail "The network probe Pods were not placed on two different Ready Nodes"
    ip_a=$(jq -r '.items[] | select(.metadata.name == "probe-a") | .status.podIP' <<< "$pods")
    ip_b=$(jq -r '.items[] | select(.metadata.name == "probe-b") | .status.podIP' <<< "$pods")
    [[ $ip_a != "$ip_b" && $ip_a =~ ^[0-9.]+$ && $ip_b =~ ^[0-9.]+$ ]] ||
        fail "The network probe Pods have invalid or equal IPv4 addresses"
    probe_http "$namespace" probe-a "$ip_b"
    probe_http "$namespace" probe-b "$ip_a"
    note "Pod-IP HTTP passed both ways between $first and $second"
)
# Scales the run's autoscaler to zero and waits for its Pods to stop.
pause_ca() {
    local result pods
    if result=$(kubectl --kubeconfig "$KUBECONFIG" --request-timeout=30s \
        -n kube-system get deployment "$PREFIX-azure-cluster-autoscaler" -o name 2>&1); then
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system scale \
            deployment/"$PREFIX-azure-cluster-autoscaler" --replicas=0 >/dev/null ||
            fail "Could not stop the run-owned autoscaler"
        pods=$(kubectl --kubeconfig "$KUBECONFIG" -n kube-system get pod \
            -l "app.kubernetes.io/instance=$PREFIX" -o name) ||
            fail "Cannot check autoscaler Pods"
        if [[ -n $pods ]]; then
            kubectl --kubeconfig "$KUBECONFIG" -n kube-system wait --for=delete pod \
                -l "app.kubernetes.io/instance=$PREFIX" --timeout=180s >/dev/null ||
                fail "The old autoscaler Pod did not stop"
        fi
    elif [[ $result != *'(NotFound)'* ]]; then
        fail "Cannot check the autoscaler Deployment before changing the fixture"
    fi
}
# Stops if a test namespace of this run still exists or cannot be listed.
no_test_namespace() {
    local namespaces
    namespaces=$(kubectl --kubeconfig "$KUBECONFIG" get namespace \
        -l "autoscaler-e2e-run=$RUN_ID" -o name) ||
        fail "Cannot list this run's test namespaces"
    [[ -z $namespaces ]] || fail "A test namespace still belongs to this run"
}
# Deletes VMSS $1 if it exists and is empty, and removes its saved model.
remove_pool() {
    local name=$1 id
    pool_exists "$name" || return 0
    id=$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")
    owned "$id"
    [[ $(pool_capacity "$name") = 0 && $(pool_actual "$name") = 0 &&
        $(pool_nics "$name") = 0 ]] ||
        fail "Pool $name still has capacity, VMs or NICs"
    az vmss delete -g "$WORKERS_RG" -n "$name" --no-wait --output none
    local i
    for ((i=0; i<60; i++)); do
        pool_exists "$name" || { rm -f "$PRIVATE_DIR/model-$name.json"; return 0; }
        sleep 10
    done
    fail "Pool $name is still present after deletion"
}
# Creates the default main and zero pools and checks Pod traffic.
ensure_pools() {
    owned_group "$WORKERS_RG"
    ensure_kubeconfig
    no_test_namespace
    ensure_pool "$PREFIX-m" main 1 2 1 Standard_D2s_v5 "$IMAGE" join
    ensure_pool "$PREFIX-z" zero 0 1 0 Standard_D2s_v5 "$IMAGE" join
    wait_pool_ready "$PREFIX-m" main 1
    [[ $(pool_actual "$PREFIX-z") = 0 ]] || fail "Zero pool is not empty"
    network_probe
}
# Creates or checks the authorization ConfigMap. Removes every allow- key,
# then sets key $1 to value $2 when $1 is not empty.
marker() {
    local key=${1:-} value=${2:-} current
    current=$(kubectl --kubeconfig "$KUBECONFIG" -n kube-system \
        get configmap autoscaler-e2e-authorization -o json 2>/dev/null) || current=
    if [[ -z $current ]]; then
        [[ -f $PRIVATE_DIR/cluster-uid ]] || fail "Cluster fingerprint is missing"
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system create configmap \
            autoscaler-e2e-authorization --from-literal="run-id=$RUN_ID" \
            --from-literal="subscription-id=$SUBSCRIPTION" \
            --from-literal="resource-group=$WORKERS_RG" \
            --from-literal="cluster-uid=$(cat "$PRIVATE_DIR/cluster-uid")" \
            --dry-run=client -o yaml |
            kubectl --kubeconfig "$KUBECONFIG" -n kube-system apply -f - >/dev/null
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system label configmap \
            autoscaler-e2e-authorization "autoscaler-e2e-run=$RUN_ID" >/dev/null
        current=$(kubectl --kubeconfig "$KUBECONFIG" -n kube-system \
            get configmap autoscaler-e2e-authorization -o json)
    fi
    jq -e --arg run "$RUN_ID" --arg sub "$SUBSCRIPTION" \
        --arg workers "$WORKERS_RG" --arg uid "$(cat "$PRIVATE_DIR/cluster-uid")" \
        '(.metadata.labels["autoscaler-e2e-run"]==$run) and
         (.data["run-id"]==$run) and (.data["subscription-id"]==$sub) and
         (.data["resource-group"]==$workers) and (.data["cluster-uid"]==$uid)' \
        <<< "$current" >/dev/null || fail "Operator marker does not match the owned cluster"
    jq --arg key "$key" --arg value "$value" \
        '.data |= with_entries(select(.key | startswith("allow-") | not)) |
         if $key=="" then . else .data[$key]=$value end |
         del(.metadata.managedFields,.status)' <<< "$current" |
        kubectl --kubeconfig "$KUBECONFIG" -n kube-system replace -f - >/dev/null
}
# Creates the run's expendable and high PriorityClasses.
priority_classes() {
    cat > "$PRIVATE_DIR/priority.yaml" <<EOF
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: $RUN_ID-expendable
  labels: {autoscaler-e2e-run: $RUN_ID}
value: -15
globalDefault: false
preemptionPolicy: PreemptLowerPriority
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: $RUN_ID-high
  labels: {autoscaler-e2e-run: $RUN_ID}
value: 1000
globalDefault: false
preemptionPolicy: PreemptLowerPriority
EOF
    kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/priority.yaml" >/dev/null
    local name
    for name in expendable high; do
        [[ $(kubectl --kubeconfig "$KUBECONFIG" get priorityclass \
            "$RUN_ID-$name" -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
            fail "PriorityClass $name is not run-owned"
    done
}
# Prints the current fixture phase.
profile() {
    [[ -f $PRIVATE_DIR/profile ]] || fail "Run pools or phase before deploying CA"
    cat "$PRIVATE_DIR/profile"
}
# Writes the JSON binding for the current phase to REPORT_DIR.
binding() {
    local phase
    phase=$(profile)
    local uid control disk
    uid=$(cat "$PRIVATE_DIR/cluster-uid")
    control=$(resource_id "$INFRA_RG" Microsoft.Compute "virtualMachines/$PREFIX-cp")
    disk=
    [[ -f $PRIVATE_DIR/disk-ready ]] && disk=$PREFIX-disk
    jq -n --arg kubeconfig "$KUBECONFIG" --arg context "$PREFIX" --arg uid "$uid" \
        --arg run "$RUN_ID" --arg sub "$SUBSCRIPTION" --arg group "$WORKERS_RG" \
        --arg control "$control" --arg location "$LOCATION" --arg image "$CA_IMAGE" \
        --arg workload "$WORKLOAD_IMAGE" --arg main "$PREFIX-m" --arg zero "$PREFIX-z" \
        --arg prefix "$PREFIX" --arg cpu "$DEMAND_CPU" --arg largeCPU "$LARGE_CPU" \
        --arg phase "$phase" --arg disk "$disk" \
        '{kubeconfig:$kubeconfig,context:$context,clusterUID:$uid,runID:$run,
          subscriptionID:$sub,resourceGroup:$group,controlPlaneID:$control,
          location:$location,discoveryValue:$run,autoscalerNamespace:"kube-system",
          autoscalerDeployment:($prefix+"-azure-cluster-autoscaler"),
          autoscalerContainer:"azure-cluster-autoscaler",leaseName:"cluster-autoscaler",
          expectedImage:$image,mainPool:$main,zeroPool:$zero,poolLabel:"acceptance-pool",
          mainLabel:"main",zeroLabel:"zero",demandCPU:(if $phase=="large" then $largeCPU else $cpu end),
          workloadImage:$workload,diskStorageClass:$disk,
          phase:(if $phase=="default" or $phase=="etag" or $phase=="taint" or
            $phase=="disk" or $phase=="dra" then "" else $phase end)}
         | if $phase=="balance" then
             .balancePoolA=($prefix+"-a") | .balancePoolB=($prefix+"-b") |
             .balanceLabel="balanced"
           elif $phase=="spot" or $phase=="spot-eviction" then
             .spotPool=($prefix+"-s") | .spotLabel="spot" |
             if $phase=="spot-eviction" then .evictSpot=true else . end
           elif $phase=="large" then
             .scalePool=($prefix+"-l") | .scaleLabel="large"
           elif $phase=="cse" then
             .failurePool=($prefix+"-f") | .failureLabel="failed" |
             .maxNodeProvisionTime="15m"
           elif $phase=="missing-vmss" then
             .missingPool=($prefix+"-x") | .deleteMissingPool=true
           elif $phase=="local-storage-true" then
             .phase="local-storage" | .skipLocalStorage=true
           elif $phase=="local-storage-false" then
             .phase="local-storage" | .skipLocalStorage=false
           elif $phase=="deallocate" or $phase=="deallocate-failed" then
             .deallocateHold="1m"
           else . end' > "$REPORT_DIR/environment.json"
}
# Writes the chart values for the autoscaler in phase $1.
ca_values() {
    local phase=$1 verbosity=4 limit=4 provision=20m local_storage=true
    case $phase in
        balance|minimum) verbosity=1 ;;
        cse) verbosity=3; provision=15m ;;
        no-join) provision=3m ;;
        large) provision=20m; limit=52 ;;
        local-storage-false) local_storage=false ;;
        deallocate) verbosity=3 ;;
        deallocate-failed) verbosity=3; provision=15m ;;
    esac
    local ca_identity tag
    ca_identity=$(az identity show -g "$INFRA_RG" -n "$PREFIX-ca" --query clientId -o tsv)
    tag=${CA_IMAGE##*:}
    [[ $CA_IMAGE = localhost/cluster-autoscaler-azure:"$tag" ]] ||
        fail "CA_IMAGE must name the exact locally loaded candidate"
    cat > "$PRIVATE_DIR/ca-values.yaml" <<EOF
autoDiscovery:
  clusterName: $RUN_ID
azureResourceGroup: $WORKERS_RG
azureSubscriptionID: $SUBSCRIPTION
azureUseManagedIdentityExtension: true
azureUserAssignedIdentityID: $ca_identity
azureVMType: vmss
azureEnableForceDelete: false
azureEnableVMSSEtag: false
replicaCount: 0
hostNetwork: true
dnsPolicy: ClusterFirstWithHostNet
image:
  repository: localhost/cluster-autoscaler-azure
  tag: $tag
  pullPolicy: Never
nodeSelector:
  kubernetes.io/hostname: $PREFIX-cp
tolerations:
- key: node-role.kubernetes.io/control-plane
  operator: Exists
  effect: NoSchedule
resources:
  requests: {cpu: 100m, memory: 256Mi}
  limits: {cpu: 500m, memory: 512Mi}
extraArgs:
  logtostderr: true
  stderrthreshold: info
  v: $verbosity
  write-status-configmap: true
  leader-elect: true
  leader-elect-resource-lock: leases
  scale-down-enabled: true
  scale-down-delay-after-add: 30s
  scale-down-unneeded-time: 30s
  scale-down-utilization-threshold: 0.5
  unremovable-node-recheck-timeout: 30s
  max-node-provision-time: $provision
  max-nodes-total: $limit
  scan-interval: 10s
  skip-nodes-with-local-storage: $local_storage
  skip-nodes-with-system-pods: true
  max-graceful-termination-sec: 120
  expendable-pods-priority-cutoff: -10
  bypassed-scheduler-names: non-existing-bypassed-scheduler
EOF
    case $phase in
        balance)
            cat >> "$PRIVATE_DIR/ca-values.yaml" <<'EOF'
  balance-similar-node-groups: true
  balancing-label: acceptance-pool
  parallel-scale-up: false
  salvo-scale-up: false
EOF
            ;;
        minimum)
            printf '  enforce-node-group-min-size: true\n' >> "$PRIVATE_DIR/ca-values.yaml" ;;
        deallocate|deallocate-failed)
            printf '  nodes: "1:2:Deallocate:%s-m"\n' "$PREFIX" >> "$PRIVATE_DIR/ca-values.yaml" ;;
    esac
    printf 'updateStrategy:\n  type: Recreate\n' >> "$PRIVATE_DIR/ca-values.yaml"
    if [[ $phase = cse || $phase = spot-eviction || $phase = deallocate* ]]; then
        printf 'extraEnv:\n' >> "$PRIVATE_DIR/ca-values.yaml"
        if [[ $phase = cse ]]; then
            printf '  AZURE_ENABLE_FAST_DELETE_ON_FAILED_PROVISIONING: "false"\n  AZURE_ENABLE_DETAILED_CSE_MESSAGE: "false"\n' \
                >> "$PRIVATE_DIR/ca-values.yaml"
        elif [[ $phase = spot-eviction ]]; then
            printf '  AZURE_GET_VMSS_SIZE_REFRESH_PERIOD: "5"\n' >> "$PRIVATE_DIR/ca-values.yaml"
        else
            # The variable overrides the cloud config, so only the per-pool
            # Deallocate spec can park VMs.
            printf '  AZURE_PROVIDER_ONLY_DEALLOCATE: "false"\n' >> "$PRIVATE_DIR/ca-values.yaml"
        fi
    fi
    if [[ $phase = etag ]]; then
        yq -i '(.azureEnableVMSSEtag) = true' "$PRIVATE_DIR/ca-values.yaml"
    fi
}
# Renders and applies the autoscaler for the current phase with zero
# replicas, then writes the binding.
ca_deploy() {
    no_test_namespace
    pause_ca
    local phase
    phase=$(profile)
    ca_values "$phase"
    helm template "$PREFIX" "$REPO_ROOT/charts/cluster-autoscaler" -n kube-system \
        -f "$PRIVATE_DIR/ca-values.yaml" > "$PRIVATE_DIR/ca-unpatched.yaml"
    local desired old
    old="--node-group-auto-discovery=label:cluster-autoscaler-enabled=true,cluster-autoscaler-name=$RUN_ID"
    desired="--node-group-auto-discovery=label:cluster-autoscaler-name=$RUN_ID"
    OLD=$old DESIRED=$desired yq eval --inplace '
      (select(.kind == "Deployment") | .spec.template.spec.containers[] |
        select(.name == "azure-cluster-autoscaler") | .command[] |
        select(. == strenv(OLD))) = strenv(DESIRED) |
      (select(.kind == "Deployment") | .spec.template.spec.containers[] |
        select(.name == "azure-cluster-autoscaler") | .env[] |
        select(.name == "ARM_SUBSCRIPTION_ID")) = {"name":"ARM_SUBSCRIPTION_ID","value":strenv(SUBSCRIPTION)} |
      (select(.kind == "Deployment") | .spec.template.spec.containers[] |
        select(.name == "azure-cluster-autoscaler") | .env[] |
        select(.name == "ARM_RESOURCE_GROUP")) = {"name":"ARM_RESOURCE_GROUP","value":strenv(WORKERS_RG)}
    ' "$PRIVATE_DIR/ca-unpatched.yaml"
    cp "$PRIVATE_DIR/ca-unpatched.yaml" "$PRIVATE_DIR/ca-rendered.yaml"
    if [[ $phase = deallocate || $phase = deallocate-failed ]]; then
        # The provider deletes the Node of each VM it parks.
        yq eval --inplace '
          (select(.kind == "ClusterRole") | .rules[] |
            select(.resources[] == "nodes") | .verbs) += ["delete"]
        ' "$PRIVATE_DIR/ca-rendered.yaml"
        [[ $(yq eval 'select(.kind == "ClusterRole") | .rules[] |
            select(.resources[] == "nodes") | .verbs[] |
            select(. == "delete")' "$PRIVATE_DIR/ca-rendered.yaml") = delete ]] ||
            fail "Controller Node delete grant was not rendered"
    fi
    [[ $(yq eval 'select(.kind == "Deployment") | .spec.replicas' "$PRIVATE_DIR/ca-rendered.yaml") = 0 &&
        $(yq eval 'select(.kind == "Deployment") | .spec.strategy.type' "$PRIVATE_DIR/ca-rendered.yaml") = Recreate ]] ||
        fail "Controller must start paused with Recreate strategy"
    [[ $(DESIRED=$desired yq eval 'select(.kind == "Deployment") |
        .spec.template.spec.containers[] | select(.name == "azure-cluster-autoscaler") |
        .command[] | select(. == strenv(DESIRED))' "$PRIVATE_DIR/ca-rendered.yaml") = "$desired" ]] ||
        fail "Controller discovery must use only the authorized run label"
    kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/ca-rendered.yaml" >/dev/null
    [[ $(kubectl --kubeconfig "$KUBECONFIG" -n kube-system get deployment \
        "$PREFIX-azure-cluster-autoscaler" -o jsonpath='{.spec.replicas}') = 0 ]] ||
        fail "Controller was started during preparation"
    binding
    note "The $phase controller is paused. Run ca-start or wait for start-controller."
}
# Starts one autoscaler replica and waits until it is Ready.
ca_start() {
    [[ $(kubectl --kubeconfig "$KUBECONFIG" -n kube-system get deployment \
        "$PREFIX-azure-cluster-autoscaler" -o jsonpath='{.spec.template.spec.containers[0].name}') = \
        azure-cluster-autoscaler ]] || fail "Controller container name changed"
    kubectl --kubeconfig "$KUBECONFIG" -n kube-system scale \
        deployment/"$PREFIX-azure-cluster-autoscaler" --replicas=1 >/dev/null
    kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status \
        deployment/"$PREFIX-azure-cluster-autoscaler" --timeout=4m >/dev/null
    [[ $(kubectl --kubeconfig "$KUBECONFIG" -n kube-system get deployment \
        "$PREFIX-azure-cluster-autoscaler" -o jsonpath='{.status.readyReplicas}') = 1 ]] ||
        fail "Exactly one controller must be Ready"
}
# Sets the main pool's min and max tags to $1 and $2 while it has one VM.
main_bounds() {
    local min=$1 max=$2 name=$PREFIX-m body=$PRIVATE_DIR/main-bounds.json
    [[ -f $PRIVATE_DIR/model-$name.json ]] ||
        fail "The saved main model is required because Azure redacts customData"
    owned "$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")"
    [[ $(pool_capacity "$name") = 1 && $(pool_actual "$name") = 1 ]] ||
        fail "Main must be physically at one before changing its bounds"
    jq --arg min "$min" --arg max "$max" \
        '.tags.min=$min | .tags.max=$max | .sku.capacity=1' \
        "$PRIVATE_DIR/model-$name.json" > "$body"
    if [[ $(az vmss show -g "$WORKERS_RG" -n "$name" -o json |
        jq --arg min "$min" --arg max "$max" \
        '.tags.min==$min and .tags.max==$max') != true ]]; then
        az rest --method put \
            --url "https://management.azure.com$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")?api-version=2024-11-01" \
            --headers Content-Type=application/json --body "@$body" --output none
        az vmss wait -g "$WORKERS_RG" -n "$name" --updated --interval 10 --timeout 900
    fi
    check_pool "$name" "$min" "$max" Standard_D2s_v5
}
# Deletes the main VMs that a deallocate case left parked, so that main
# returns to its one running worker.
remove_parked_main() {
    local name=$PREFIX-m ids count
    owned "$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")"
    ids=$(az vmss list-instances -g "$WORKERS_RG" -n "$name" --expand instanceView -o json |
        jq -r '.[] | select([.instanceView.statuses[]?.code] | index("PowerState/deallocated")) | .instanceId')
    [[ -n $ids ]] || return 0
    count=$(wc -w <<< "$ids")
    (( $(pool_actual "$name") - count == 1 )) ||
        fail "Main must keep exactly one worker besides its parked VMs"
    note "Deleting parked main instances: $(tr '\n' ' ' <<< "$ids")"
    # Instance IDs are numbers, so word splitting passes one argument each.
    # shellcheck disable=SC2086
    az vmss delete-instances -g "$WORKERS_RG" -n "$name" --instance-ids $ids --output none
}
# Keeps the zero pool if it has max $1, the no-join tag of special mode $3
# and taint $4. Otherwise recreates it with bootstrap $2.
zero_model() {
    local max=$1 bootstrap=$2 special=${3:-normal} taint=${4:-} name=$PREFIX-z
    if pool_exists "$name"; then
        check_pool "$name" 0 "$(az vmss show -g "$WORKERS_RG" -n "$name" --query tags.max -o tsv)" Standard_D2s_v5
        [[ $(pool_capacity "$name") = 0 && $(pool_actual "$name") = 0 &&
            $(pool_nics "$name") = 0 ]] || fail "Zero pool is not physically empty"
        local current
        current=$(az vmss show -g "$WORKERS_RG" -n "$name" -o json)
        if [[ $(jq -r '.tags.max' <<< "$current") = "$max" &&
            $(jq -r '.tags["k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run"] // ""' <<< "$current") = "$taint" &&
            $(jq -r '.tags["autoscaler-e2e-no-join"] // ""' <<< "$current") = \
                "$([[ $special = no-join ]] && printf '%s' "$RUN_ID")" ]]; then
            [[ -f $PRIVATE_DIR/model-$name.json ]] ||
                fail "The private zero model is missing; Azure redacts customData"
            return
        fi
        remove_pool "$name"
    fi
    POOL_TAINT=$taint ensure_pool "$name" zero 0 "$max" 0 Standard_D2s_v5 \
        "$([[ $bootstrap = join ]] && printf '%s' "$IMAGE" || printf 'Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest')" \
        "$bootstrap" "$special"
}
# Pauses the autoscaler and changes the pools, marker and binding to fixture
# phase $1.
phase() {
    local name=$1 min=1 max=2 zero_max=0 bootstrap=join zero_special=normal taint=
    local marker_key='' marker_value=''
    local retained=
    case $name in
        default|etag|taint|disk|dra)
            zero_max=1
            [[ $name = taint ]] && taint="$RUN_ID:NoSchedule"
            # disk up and dra up set their markers once the addon is ready.
            [[ $name = default ]] && { marker_key=allow-kube-system-fixture; marker_value=CA-011; }
            ;;
        balance)
            max=1; retained='a b'; marker_key=allow-balance-fixture; marker_value=AZ-P1-007 ;;
        no-join)
            zero_max=1; bootstrap=nojoin; zero_special=no-join
            marker_key=allow-no-join-fixture; marker_value=AZ-P1-008 ;;
        minimum)
            min=2; zero_max=1; max=2
            marker_key=allow-minimum-fixture; marker_value=AZ-P1-009 ;;
        spot) retained=s; marker_key=allow-spot-fixture; marker_value=AZ-P1-010 ;;
        large)
            max=1; retained=l; marker_key=allow-large-fixture; marker_value=AZ-P1-011 ;;
        cse) retained=f; marker_key=allow-cse-fixture; marker_value=AZ-P1-012 ;;
        spot-eviction)
            retained=s; marker_key=allow-spot-eviction-fixture; marker_value=AZ-P1-013 ;;
        missing-vmss)
            retained=x; marker_key=allow-missing-vmss-fixture; marker_value=AZ-P1-014 ;;
        local-storage-true|local-storage-false)
            marker_key=allow-local-storage-fixture; marker_value=AZ-P1-015 ;;
        deallocate) marker_key=allow-deallocate-fixture; marker_value=AZ-P3-001 ;;
        deallocate-failed)
            marker_key=allow-deallocate-failed-fixture; marker_value=AZ-P3-002 ;;
        *) fail "Unknown phase: $name" ;;
    esac
    ensure_kubeconfig
    no_test_namespace
    pause_ca
    if [[ -f $PRIVATE_DIR/large-start && $name != large ]]; then
        local prior duration spent=0
        prior=$(cat "$PRIVATE_DIR/large-start")
        duration=$(( $(date -u +%s) - $(epoch "$prior") ))
        [[ -f $PRIVATE_DIR/large-seconds ]] && spent=$(cat "$PRIVATE_DIR/large-seconds")
        printf '%s\n' "$((spent + duration))" > "$PRIVATE_DIR/large-seconds"
        rm -f "$PRIVATE_DIR/large-start"
    fi
    local extra
    for extra in a b s l f x; do
        if [[ -f $PRIVATE_DIR/profile && $(profile) = "$name" &&
            " $retained " = *" $extra "* ]]; then
            continue
        fi
        remove_pool "$PREFIX-$extra"
    done
    if [[ -f $PRIVATE_DIR/profile && $(profile) = deallocate* ]]; then
        remove_parked_main
    fi
    main_bounds "$min" "$max"
    zero_model "$zero_max" "$bootstrap" "$zero_special" "$taint"
    case $name in
        balance)
            ensure_pool "$PREFIX-a" balanced 0 2 0 Standard_D2s_v5 "$IMAGE" join
            ensure_pool "$PREFIX-b" balanced 0 2 0 Standard_D2s_v5 "$IMAGE" join
            ;;
        spot|spot-eviction)
            ensure_pool "$PREFIX-s" spot 0 1 \
                "$([[ $name = spot-eviction ]] && printf 1 || printf 0)" \
                Standard_D2s_v5 "$IMAGE" join spot
            [[ $name = spot-eviction ]] && wait_pool_ready "$PREFIX-s" spot 1
            ;;
        large)
            large_quota
            [[ -f $PRIVATE_DIR/large-start ]] ||
                date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PRIVATE_DIR/large-start"
            ensure_pool "$PREFIX-l" large 0 50 1 Standard_B1ms "$IMAGE" join
            wait_pool_ready "$PREFIX-l" large 1
            note "Large-pool Node allocatable and current Pod requests must support LARGE_CPU=$LARGE_CPU"
            ;;
        cse)
            ensure_pool "$PREFIX-f" failed 0 1 0 Standard_D2s_v5 \
                Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest nojoin cse
            ;;
        missing-vmss)
            ensure_pool "$PREFIX-x" missing 0 0 0 Standard_D2s_v5 \
                Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest nojoin
            ;;
    esac
    printf '%s\n' "$name" > "$PRIVATE_DIR/profile"
    marker "$marker_key" "$marker_value"
    binding
    tag_created_resources
    network_probe
    note "$name is prepared with CA paused; run ca-deploy next"
}
# Stops unless regional and B-series quota leave room for 50 B1ms VMs.
large_quota() {
    local used max_cores b_used b_max quota
    quota=$(az vm list-usage --location "$LOCATION" -o json)
    used=$(jq -r \
        '[.[]|select(.name.localizedValue=="Total Regional vCPUs")|.currentValue][0]' <<< "$quota")
    max_cores=$(jq -r \
        '[.[]|select(.name.localizedValue=="Total Regional vCPUs")|.limit][0]' <<< "$quota")
    b_used=$(jq -r '[.[]|select(.name.localizedValue | test("Standard B(S)? Family vCPUs"))|.currentValue][0]' <<< "$quota")
    b_max=$(jq -r '[.[]|select(.name.localizedValue | test("Standard B(S)? Family vCPUs"))|.limit][0]' <<< "$quota")
    [[ $used =~ ^[0-9]+$ && $max_cores =~ ^[0-9]+$ &&
        $b_used =~ ^[0-9]+$ && $b_max =~ ^[0-9]+$ ]] ||
        fail "Regional and B-series quota checks must return core counts"
    (( max_cores - used >= 50 && b_max - b_used >= 50 )) ||
        fail "Not enough regional or B-series quota for fifty B1ms workers"
}
# Installs or removes the synthetic DRA driver. $1 is up or down.
dra() {
    local action=$1
    [[ $(profile) = dra ]] || fail "Select phase dra before changing its addon"
    no_test_namespace
    pause_ca
    sed "s/@RUN_ID@/$RUN_ID/g" "$HERE/dra-driver.yaml.in" > "$PRIVATE_DIR/dra-driver.yaml"
    case $action in
        up)
            if kubectl --kubeconfig "$KUBECONFIG" get deviceclass gpu >/dev/null 2>&1; then
                [[ $(kubectl --kubeconfig "$KUBECONFIG" get deviceclass gpu \
                    -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
                    fail "An existing DeviceClass gpu belongs to another run"
            fi
            kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/dra-driver.yaml" >/dev/null
            kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status \
                daemonset/dra-example-driver-kubeletplugin --timeout=6m >/dev/null
            [[ $(kubectl --kubeconfig "$KUBECONFIG" get deviceclass gpu \
                -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
                fail "DRA DeviceClass is not run-owned"
            local i slices
            for ((i=0; i<45; i++)); do
                slices=$(kubectl --kubeconfig "$KUBECONFIG" get resourceslices -o json)
                if jq -e '[.items[] | select(.spec.driver=="gpu.example.com" and
                    (.spec.devices|length)==4)] | length >= 1' <<< "$slices" >/dev/null; then
                    break
                fi
                sleep 5
            done
            jq -e '[.items[] | select(.spec.driver=="gpu.example.com" and
                (.spec.devices|length)==4)] | length >= 1' <<< "$slices" >/dev/null ||
                fail "DRA did not publish four devices on a worker"
            marker allow-dra-fixture CA-020,CA-021,CA-022
            ;;
        down)
            kubectl --kubeconfig "$KUBECONFIG" delete -f "$PRIVATE_DIR/dra-driver.yaml" \
                --ignore-not-found >/dev/null
            local remaining i
            for ((i=0; i<45; i++)); do
                remaining=$(kubectl --kubeconfig "$KUBECONFIG" get resourceslices -o json |
                    jq '[.items[]|select(.spec.driver=="gpu.example.com")]|length')
                [[ $remaining = 0 ]] && break
                sleep 5
            done
            [[ $remaining = 0 ]] || fail "DRA ResourceSlices still exist"
            marker
            ;;
        *) fail "Use dra up or dra down" ;;
    esac
}
# Installs or removes Azure Disk CSI and its StorageClass. $1 is up or down.
disk() {
    local action=$1
    [[ $(profile) = disk ]] || fail "Select phase disk before changing its addon"
    no_test_namespace
    pause_ca
    local class=$PREFIX-disk
    case $action in
        up)
            required DISK_CHART
            [[ -f $DISK_CHART ]] || fail "A pinned Azure Disk CSI chart archive is required"
            [[ $(helm show chart "$DISK_CHART" | yq eval '.version' -) = 1.36.0 ]] ||
                fail "The Azure Disk CSI chart must be version 1.36.0"
            if kubectl --kubeconfig "$KUBECONFIG" get storageclass "$class" >/dev/null 2>&1; then
                [[ $(kubectl --kubeconfig "$KUBECONFIG" get storageclass "$class" \
                    -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
                    fail "An existing disk StorageClass belongs to another run"
            fi
            cat > "$PRIVATE_DIR/disk-values.yaml" <<EOF
controller:
  replicas: 1
  runOnControlPlane: true
  nodeSelector:
    kubernetes.io/hostname: $PREFIX-cp
linux:
  nodeSelector:
    acceptance-pool: main
EOF
            helm template "$PREFIX-disk-csi" "$DISK_CHART" -n kube-system \
                -f "$PRIVATE_DIR/disk-values.yaml" > "$PRIVATE_DIR/disk-rendered.yaml"
            kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/disk-rendered.yaml" >/dev/null
            cat > "$PRIVATE_DIR/storageclass.yaml" <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $class
  labels: {autoscaler-e2e-run: $RUN_ID}
provisioner: disk.csi.azure.com
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
parameters:
  skuName: StandardSSD_LRS
  subscriptionID: $SUBSCRIPTION
  resourceGroup: $WORKERS_RG
  tags: autoscaler-e2e-run=$RUN_ID
EOF
            kubectl --kubeconfig "$KUBECONFIG" apply -f "$PRIVATE_DIR/storageclass.yaml" >/dev/null
            [[ $(kubectl --kubeconfig "$KUBECONFIG" get storageclass "$class" \
                -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
                fail "Disk StorageClass is not run-owned"
            kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status \
                deployment/csi-azuredisk-controller --timeout=8m >/dev/null ||
                fail "Azure Disk CSI controller did not become Ready"
            kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status \
                daemonset/csi-azuredisk-node --timeout=8m >/dev/null ||
                fail "Azure Disk CSI worker plugin did not become Ready"
            touch "$PRIVATE_DIR/disk-ready"
            marker allow-disk-fixture AZ-P1-006
            binding
            ;;
        down)
            [[ $(az disk list -g "$WORKERS_RG" --query 'length(@)' -o tsv) = 0 ]] ||
                fail "Run-owned data disks must be physically gone before removing CSI"
            if kubectl --kubeconfig "$KUBECONFIG" get storageclass "$class" >/dev/null 2>&1; then
                [[ $(kubectl --kubeconfig "$KUBECONFIG" get storageclass "$class" \
                    -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
                    fail "Disk StorageClass belongs to another run"
                kubectl --kubeconfig "$KUBECONFIG" delete storageclass "$class" >/dev/null
            fi
            [[ -f $PRIVATE_DIR/disk-rendered.yaml ]] &&
                kubectl --kubeconfig "$KUBECONFIG" delete -f "$PRIVATE_DIR/disk-rendered.yaml" \
                    --ignore-not-found >/dev/null
            rm -f "$PRIVATE_DIR/disk-ready"
            marker
            binding
            ;;
        *) fail "Use disk up or disk down" ;;
    esac
}
# Prints the data of the one run-owned signal ConfigMap named $1.
signal_data() {
    local signal=$1 namespace items
    items=$(kubectl --kubeconfig "$KUBECONFIG" get configmaps -A \
        -l "autoscaler-e2e-run=$RUN_ID" -o json)
    [[ $(jq --arg signal "$signal" \
        '[.items[]|select(.metadata.name==$signal)]|length' <<< "$items") = 1 ]] ||
        fail "Expected exactly one $signal ConfigMap from this run"
    namespace=$(jq -r --arg signal "$signal" \
        '.items[]|select(.metadata.name==$signal)|.metadata.namespace' <<< "$items")
    [[ $namespace = azure-e2e-* &&
        $(kubectl --kubeconfig "$KUBECONFIG" get namespace "$namespace" \
            -o jsonpath='{.metadata.labels.autoscaler-e2e-run}') = "$RUN_ID" ]] ||
        fail "Signal is outside a run-owned test namespace"
    jq --arg signal "$signal" \
        '.items[]|select(.metadata.name==$signal)|.data' <<< "$items"
}
# Acts on a signal that a paused spec created. $1 is start, missing or spot.
signal() {
    local action=$1 data expected actual set vmid instance
    case $action in
        start)
            case $(profile) in balance|large|minimum) ;; *) fail "Current phase cannot signal start" ;; esac
            signal_data start-controller >/dev/null
            [[ $(profile) = large ]] && large_quota
            ca_start
            ;;
        missing)
            [[ $(profile) = missing-vmss ]] || fail "Current phase is not missing-vmss"
            data=$(signal_data delete-missing-vmss)
            expected=$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$PREFIX-x")
            jq -e --arg id "$expected" --arg run "$RUN_ID" \
                '.["resource-id"]==$id and .["run-id"]==$run' <<< "$data" >/dev/null ||
                fail "Missing-VMSS signal has a foreign ID or run"
            owned "$expected"
            set=$(az vmss show -g "$WORKERS_RG" -n "$PREFIX-x" -o json)
            jq -e --arg id "$expected" --arg run "$RUN_ID" \
                --arg deadline "$DEADLINE" \
                '(.id|ascii_downcase)==($id|ascii_downcase) and
                 .tags.runID==$run and .tags.cleanupDeadlineUTC==$deadline and
                 .tags["autoscaler-e2e-run"]==$run and
                 .tags["cluster-autoscaler-name"]==$run and
                 .tags.min=="0" and .tags.max=="0" and .sku.capacity==0' \
                <<< "$set" >/dev/null || fail "Missing VMSS is not the authorized empty group"
            [[ $(pool_actual "$PREFIX-x") = 0 && $(pool_nics "$PREFIX-x") = 0 ]] ||
                fail "Missing VMSS still has a VM or NIC"
            az vmss delete -g "$WORKERS_RG" -n "$PREFIX-x" --no-wait --output none
            local i
            for ((i=0; i<60; i++)); do pool_exists "$PREFIX-x" || break; sleep 10; done
            pool_exists "$PREFIX-x" && fail "Missing VMSS was not deleted"
            ;;
        spot)
            [[ $(profile) = spot-eviction ]] || fail "Current phase is not spot-eviction"
            data=$(signal_data evict-spot-vm)
            vmid=$(jq -r '.["vm-id"]' <<< "$data")
            actual=$(jq -r '.["resource-id"]' <<< "$data")
            expected=$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$PREFIX-s")
            actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
            expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
            instance=${actual##*/}
            [[ $instance =~ ^[0-9]+$ &&
                $actual = "$expected/virtualmachines/$instance" &&
                $vmid =~ ^[0-9a-fA-F-]{36}$ ]] ||
                fail "Spot signal does not identify an instance of this run"
            [[ $(jq -r '.["run-id"]' <<< "$data") = "$RUN_ID" ]] ||
                fail "Spot signal belongs to another run"
            owned "$expected"
            set=$(az vmss show -g "$WORKERS_RG" -n "$PREFIX-s" -o json)
            jq -e --arg run "$RUN_ID" --arg deadline "$DEADLINE" \
                '.tags.runID==$run and .tags.cleanupDeadlineUTC==$deadline and
                 .virtualMachineProfile.priority=="Spot" and
                 .virtualMachineProfile.evictionPolicy=="Delete" and .sku.capacity==1' \
                <<< "$set" >/dev/null || fail "Spot VMSS differs from the approved fixture"
            [[ $(az vmss show -g "$WORKERS_RG" -n "$PREFIX-s" \
                --instance-id "$instance" --query vmId -o tsv) = "$vmid" ]] ||
                fail "The signaled Spot VM was replaced; do not evict another VM"
            # The backticks are JMESPath literals, not shell expansions.
            # shellcheck disable=SC2016
            [[ $(az vmss get-instance-view -g "$WORKERS_RG" -n "$PREFIX-s" \
                --instance-id "$instance" --query 'statuses[?code==`PowerState/running`]|length(@)' -o tsv) = 1 ]] ||
                fail "The exact Spot VM is not Running"
            az vmss simulate-eviction -g "$WORKERS_RG" -n "$PREFIX-s" \
                --instance-id "$instance" --output none
            ;;
        *) fail "Use signal start, signal missing or signal spot" ;;
    esac
    note "$action signal was handled for $RUN_ID"
}
# Prints the estimated run cost in USD, including the cleanup reserve.
estimated_cost() {
    local now started large_start extra elapsed large_elapsed
    now=$(date -u +%s)
    started=$(epoch "$(cat "$PRIVATE_DIR/started-at")")
    elapsed=$((now - started))
    extra=0
    [[ -f $PRIVATE_DIR/large-seconds ]] && extra=$(cat "$PRIVATE_DIR/large-seconds")
    if [[ -f $PRIVATE_DIR/large-start ]]; then
        large_start=$(epoch "$(cat "$PRIVATE_DIR/large-start")")
        extra=$((extra + now - large_start))
    fi
    large_elapsed=$extra
    awk -v elapsed="$elapsed" -v large="$large_elapsed" \
        -v reserve="$CLEANUP_RESERVE_USD" \
        'BEGIN { printf "%.2f", 1.25*elapsed/3600 + large/3600 + reserve }'
}
# Stops when the deadline, the budget or the ten-hour window is spent.
check_budget() {
    [[ -f $PRIVATE_DIR/started-at ]] || return 0
    local now started estimate end
    now=$(date -u +%s)
    started=$(epoch "$(cat "$PRIVATE_DIR/started-at")")
    end=$(epoch "$DEADLINE")
    (( now < end )) || fail "Cleanup deadline passed; run down now"
    estimate=$(estimated_cost)
    awk -v estimate="$estimate" -v budget="$RUN_BUDGET_USD" \
        -v past="${PAST_RUN_COST_USD:-0}" \
        'BEGIN { if (estimate > budget || estimate + past > 30) exit 1 }' ||
        fail "The run or combined USD budget is exhausted; stop new work and run down"
    (( now - started < 3600 * 10 )) ||
        fail "The ten-hour run window is over; run down"
}
# Checks the budget, the NSG and the VM and vCPU caps every 30 seconds.
watch() {
    [[ -f $PRIVATE_DIR/started-at ]] || fail "No owned run has started"
    printf '%s\n' "$$" > "$PRIVATE_DIR/watch.pid"
    local groups name capacity actual size vms cores max_vms max_cores
    while :; do
        check_budget
        verify_operator_nsg
        vms=1
        cores=2
        groups=$(az vmss list -g "$WORKERS_RG" -o json)
        while IFS=$'\t' read -r name capacity size; do
            [[ -n $name ]] || continue
            owned "$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$name")"
            actual=$(pool_actual "$name")
            if (( actual > capacity )); then capacity=$actual; fi
            vms=$((vms + capacity))
            case $size in
                Standard_D2s_v5) cores=$((cores + 2*capacity)) ;;
                Standard_B1ms)
                    [[ $(profile) = large ]] || fail "B1ms outside the large phase"
                    cores=$((cores + capacity)) ;;
                *) fail "Unexpected VM size $size" ;;
            esac
        done < <(jq -r '.[]|[.name,.sku.capacity,.sku.name]|@tsv' <<< "$groups")
        max_vms=4
        max_cores=8
        [[ $(profile) = large ]] && { max_vms=55; max_cores=60; }
        note "profile=$(profile) VMs=$vms vCPUs=$cores estimate=USD$(estimated_cost)"
        (( vms <= max_vms && cores <= max_cores )) ||
            fail "The observed VM or vCPU cap was exceeded; stop CA and run down"
        sleep 30
    done
}
# Runs case $1 with its own report directory after checking the phase, the
# budget and the watch process.
run_case() {
    local label=$1 phase timeout suite timeout_minutes
    [[ $label =~ ^(AZ-P1-0(0[1-9]|1[0-5])|AZ-P3-00[12]|AZ-SUP-ETAG|AZ-001|CA-0(0[1-2]|0[4-9]|1[0-9]|2[0-2]))$ ]] ||
        fail "Unknown maintained E2E case ID"
    no_test_namespace
    check_budget
    verify_operator_nsg
    if [[ ! -f $PRIVATE_DIR/watch.pid ]] ||
        ! kill -0 "$(cat "$PRIVATE_DIR/watch.pid")" 2>/dev/null; then
        fail "Start an attached watch process before running a case"
    fi
    [[ $(ps -p "$(cat "$PRIVATE_DIR/watch.pid")" -o args=) = *"fixture.sh $ENV_FILE watch"* ]] ||
        fail "The saved watch PID is not this run's cap monitor"
    phase=$(profile)
    suite=scaleup
    timeout_minutes=90
    case $label in AZ-P1-00[7-9]|AZ-P1-01[0-5]|AZ-P3-00[12]) suite=scalephase ;; esac
    case $label in
        AZ-P1-005) [[ $phase = taint ]] || fail "Select phase taint first" ;;
        AZ-P1-006) [[ $phase = disk && -f $PRIVATE_DIR/disk-ready ]] ||
            fail "Select phase disk and install the CSI driver first" ;;
        CA-020|CA-021|CA-022)
            [[ $phase = dra ]] || fail "Select phase dra first" ;;
        AZ-SUP-ETAG) [[ $phase = etag ]] || fail "Select phase etag first" ;;
        AZ-P1-007) [[ $phase = balance ]] || fail "Select phase balance first" ;;
        AZ-P1-008) [[ $phase = no-join ]] || fail "Select phase no-join first" ;;
        AZ-P1-009) [[ $phase = minimum ]] || fail "Select phase minimum first" ;;
        AZ-P1-010) [[ $phase = spot ]] || fail "Select phase spot first" ;;
        AZ-P1-011) [[ $phase = large ]] || fail "Select phase large first"; timeout_minutes=300 ;;
        AZ-P1-012) [[ $phase = cse ]] || fail "Select phase cse first"; timeout_minutes=100 ;;
        AZ-P1-013) [[ $phase = spot-eviction ]] || fail "Select phase spot-eviction first" ;;
        AZ-P1-014) [[ $phase = missing-vmss ]] || fail "Select phase missing-vmss first" ;;
        AZ-P1-015) [[ $phase = local-storage-true ||
            $phase = local-storage-false ]] || fail "Select a local-storage phase first" ;;
        AZ-P3-001) [[ $phase = deallocate ]] || fail "Select phase deallocate first" ;;
        AZ-P3-002) [[ $phase = deallocate-failed ]] ||
            fail "Select phase deallocate-failed first"; timeout_minutes=100 ;;
        *) [[ $phase = default ]] || fail "Run ordinary specs only on the default fixture" ;;
    esac
    local started now
    now=$(date -u +%s)
    started=$(epoch "$(cat "$PRIVATE_DIR/started-at")")
    (( now - started < STOP_NEW_HOURS * 3600 )) ||
        fail "The new-case stop time has passed; run down"
    local remaining=$(( 10*3600 - (now-started) - 3600 ))
    local deadline deadline_remaining
    deadline=$(epoch "$DEADLINE")
    deadline_remaining=$(( deadline - now - 3600 ))
    if (( deadline_remaining < remaining )); then remaining=$deadline_remaining; fi
    (( remaining >= 15*60 )) || fail "The case cannot leave an hour for cleanup"
    if (( timeout_minutes*60 > remaining )); then
        timeout_minutes=$(( remaining/60 ))
    fi
    timeout=${timeout_minutes}m
    [[ $(date -u +%s) -lt $(epoch "$DEADLINE") ]] ||
        fail "Cleanup deadline has passed"
    local attempt=${RUN_ATTEMPT:-1}
    [[ $attempt =~ ^[a-zA-Z0-9-]+$ ]] || fail "RUN_ATTEMPT must be a simple name"
    local dir=$REPORT_DIR/runs/$label-$attempt
    [[ $phase = local-storage-* ]] && dir=$dir-${phase#local-storage-}
    [[ ! -e $dir ]] || fail "A case report already exists; set a new RUN_ATTEMPT for a retry"
    if [[ $phase = spot-eviction ]]; then ca_start; fi
    mkdir -p "$dir"
    note "Running $label on $phase with its own report"
    (cd "$MODULE_DIR" && env -u AZURE_CLIENT_ID -u AZURE_CLIENT_SECRET \
        -u AZURE_TENANT_ID -u AZURE_CLIENT_ID_USER_ASSIGNED_IDENTITY \
        make e2etests TEST_SUITE="$suite" LABEL_FILTER="$label" \
        ENVIRONMENT="$REPORT_DIR/environment.json" ARTIFACTS="$dir" \
        TEST_TIMEOUT="$timeout") > "$dir/output.log" 2>&1 || {
        tail -n 24 "$dir/output.log" >&2
        fail "$label failed; fix only the affected fixture step or stop for a design change"
    }
    jq -e --arg label "$label" '[.[].SpecReports[] |
        select((.LeafNodeLabels|index($label)) != null and .State=="passed")] | length == 1' \
        "$dir"/report.*.json >/dev/null ||
        fail "$label has no single passing Ginkgo report"
    note "$label passed"
}
# Builds and saves the candidate image archive once, and records its ID.
ensure_image() {
    local tag=${CA_IMAGE##*:} id
    [[ $CA_IMAGE = localhost/cluster-autoscaler-azure:"$tag" ]] ||
        fail "Only the local candidate image is allowed"
    if [[ ! -f $PRIVATE_DIR/ca-amd64.tar ]]; then
        (cd "$REPO_ROOT" && make image GOARCH=amd64 \
            IMAGE=localhost/cluster-autoscaler-azure TAG="$tag") > "$REPORT_DIR/image-build.log" 2>&1 ||
            fail "Build failed; see the local non-secret image log"
        if ! docker save --output "$PRIVATE_DIR/ca-amd64.tar.tmp" "$CA_IMAGE"; then
            rm -f "$PRIVATE_DIR/ca-amd64.tar.tmp"
            fail "Cannot save the candidate image archive"
        fi
        mv -f "$PRIVATE_DIR/ca-amd64.tar.tmp" "$PRIVATE_DIR/ca-amd64.tar"
    fi
    id=$(docker image inspect "$CA_IMAGE" --format '{{.Id}}')
    [[ -n $id && $(docker image inspect "$CA_IMAGE" --format '{{.Architecture}}') = amd64 ]] ||
        fail "Candidate image does not have the required amd64 architecture"
    printf '%s\n' "$id" > "$REPORT_DIR/runtime-image-id"
}
# Stops unless the run's groups and every run-tagged or prefixed resource
# are gone.
verify_cleanup_inventory() {
    local left
    [[ $(az group exists -n "$WORKERS_RG") = false &&
        $(az group exists -n "$INFRA_RG") = false ]] ||
        fail "Run-owned resource groups remain"
    left=$(az resource list --tag "runID=$RUN_ID" --query 'length(@)' -o tsv)
    [[ $left = 0 ]] || fail "Run-tagged Azure resources remain"
    left=$(az resource list -o json | jq --arg run "$RUN_ID" --arg prefix "$PREFIX-" \
        '[.[]|select((.id|ascii_downcase|contains($run|ascii_downcase)) or
                    (.name|ascii_downcase|startswith($prefix|ascii_downcase)))]|length')
    [[ $left = 0 ]] || fail "Run-prefixed Azure resources remain"
}
# Deletes run-owned resource group $1 and waits until it is gone.
delete_group() {
    local group=$1 i
    if [[ $(az group exists -n "$group") = false ]]; then return; fi
    owned_group "$group"
    if [[ $(az group show -n "$group" --query properties.provisioningState -o tsv) != Deleting ]]; then
        az group delete -n "$group" --yes --no-wait
    fi
    for ((i=0; i<90; i++)); do
        [[ $(az group exists -n "$group") = false ]] && return
        sleep 20
    done
    fail "$group did not disappear"
}
# Stops the run's tunnel processes and checks that their ports are free.
stop_tunnels() {
    local kind pid args port resource listener i
    for kind in ssh api; do
        [[ -f $PRIVATE_DIR/tunnel-$kind.pid ]] || continue
        case $kind in
            ssh) port=$SSH_PORT; resource=22 ;;
            api) port=$API_PORT; resource=6443 ;;
        esac
        pid=$(cat "$PRIVATE_DIR/tunnel-$kind.pid")
        if kill -0 "$pid" 2>/dev/null; then
            args=$(ps -p "$pid" -o args=)
            tunnel_args_match "$args" "$resource" "$port" ||
                fail "Saved $kind tunnel PID belongs to another process"
            kill "$pid"
        fi
        for ((i=0; i<10; i++)); do
            listener=$(tunnel_listeners "$port")
            [[ -n $listener ]] || break
            while IFS= read -r pid; do
                args=$(ps -p "$pid" -o args=)
                tunnel_args_match "$args" "$resource" "$port" ||
                    fail "Local port $port belongs to another process"
                kill "$pid"
            done <<< "$listener"
            sleep 1
        done
        listener=$(tunnel_listeners "$port")
        [[ -z $listener ]] || fail "The $kind tunnel still holds local port $port"
        rm -f "$PRIVATE_DIR/tunnel-$kind.pid"
    done
}
# Reports whether process arguments $1 are this run's tunnel to port $2 on
# local port $3.
tunnel_args_match() {
    local args=" $1 " target
    target=$(resource_id "$INFRA_RG" Microsoft.Compute "virtualMachines/$PREFIX-cp")
    [[ $args = *" network bastion tunnel "* &&
        $args = *" --name $PREFIX-bastion "* &&
        $args = *" -g $INFRA_RG "* &&
        $args = *" --target-resource-id $target "* &&
        $args = *" --resource-port $2 "* &&
        $args = *" --port $3 "* ]]
}
# Prints the PIDs that listen on local TCP port $1. Stops when lsof fails.
tunnel_listeners() {
    local output status=0
    output=$(lsof -nP -tiTCP:"$1" -sTCP:LISTEN 2>&1) || status=$?
    (( status == 0 || status == 1 )) || fail "Cannot inspect local tunnel port $1"
    if (( status == 1 )) && [[ -n $output ]]; then
        fail "Cannot inspect local tunnel port $1"
    fi
    [[ -z $output || $output =~ ^[0-9]+($'\n'[0-9]+)*$ ]] ||
        fail "Cannot inspect local tunnel port $1"
    printf '%s\n' "$output"
}
# Stops the tunnels, records the cost, removes the candidate image and
# deletes the private files.
local_cleanup() {
    [[ -d $PRIVATE_DIR ]] || return 0
    [[ ${PRIVATE_DIR##*/} = "$RUN_ID" && ! -L $PRIVATE_DIR ]] ||
        fail "Refusing to remove an unsafe private directory"
    stop_tunnels
    [[ -f $PRIVATE_DIR/started-at ]] &&
        estimated_cost > "$REPORT_DIR/run-cost-usd"
    if [[ -f $REPORT_DIR/runtime-image-id ]] &&
        docker image inspect "$CA_IMAGE" >/dev/null 2>&1; then
        [[ $(docker image inspect "$CA_IMAGE" --format '{{.Id}}') = \
            "$(cat "$REPORT_DIR/runtime-image-id")" ]] ||
            fail "The candidate image tag changed owners"
        docker image rm "$CA_IMAGE" >/dev/null
    fi
    find "$PRIVATE_DIR" -mindepth 1 -maxdepth 1 -type f -delete
    rmdir "$PRIVATE_DIR"
    [[ ! -e $PRIVATE_DIR ]] || fail "Private run files remain"
}
# Deletes the saved role assignments that still belong to the run's
# identities.
remove_roles() {
    local id scope identity principal assignment owner
    [[ -f $PRIVATE_DIR/roles || -f $PRIVATE_DIR/roles.tmp ]] || return
    while read -r id scope identity; do
        [[ -n $id ]] || continue
        [[ $scope = "/subscriptions/$SUBSCRIPTION/resourceGroups/$WORKERS_RG" ||
            $scope = "/subscriptions/$SUBSCRIPTION/resourceGroups/$INFRA_RG" ]] ||
            fail "Saved role assignment is outside this run"
        [[ $id = "$scope/providers/Microsoft.Authorization/roleAssignments/"* ]] ||
            fail "Saved role ID does not belong to its resource group"
        assignment=$(az role assignment list --scope "$scope" -o json |
            jq --arg id "$id" '[.[]|select((.id|ascii_downcase)==($id|ascii_downcase))]')
        [[ $(jq 'length' <<< "$assignment") = 0 ]] && continue
        [[ $(jq 'length' <<< "$assignment") = 1 ]] || fail "Duplicate saved role IDs"
        owner=$(jq -r '.[0].principalId' <<< "$assignment")
        principal=$(az identity show -g "$INFRA_RG" -n "$identity" --query principalId -o tsv)
        [[ $owner = "$principal" ]] || fail "Saved role assignment has another principal"
        az role assignment delete --ids "$id" ||
            fail "Could not remove a run-created role assignment"
    done < "${PRIVATE_DIR}/$( [[ -f $PRIVATE_DIR/roles ]] && printf roles || printf roles.tmp )"
}
# Stops unless every saved role assignment is gone.
verify_roles_absent() {
    local id scope identity short remaining
    [[ -f $PRIVATE_DIR/roles || -f $PRIVATE_DIR/roles.tmp ]] || return 0
    while read -r id scope identity; do
        [[ -n $id ]] || continue
        short=${id##*/}
        [[ $short =~ ^[0-9a-fA-F-]{36}$ &&
            ( $scope = "/subscriptions/$SUBSCRIPTION/resourceGroups/$WORKERS_RG" ||
              $scope = "/subscriptions/$SUBSCRIPTION/resourceGroups/$INFRA_RG" ) ]] ||
            fail "Saved role assignment is outside this run"
        remaining=$(az role assignment list --all \
            --query "[?name=='$short'] | length(@)" -o tsv)
        [[ $remaining = 0 ]] || fail "Run-created role assignment $short remains"
    done < "${PRIVATE_DIR}/$( [[ -f $PRIVATE_DIR/roles ]] && printf roles || printf roles.tmp )"
}
# Pauses the autoscaler, deletes the run's test namespaces and revokes the
# bootstrap tokens.
cleanup_cluster() {
    ensure_kubeconfig
    pause_ca || fail "Could not stop the controller before in-cluster cleanup"
    local namespaces namespace secrets
    namespaces=$(kubectl --kubeconfig "$KUBECONFIG" --request-timeout=30s \
        get namespace -l "autoscaler-e2e-run=$RUN_ID" -o json) ||
        fail "Cannot list run-owned test namespaces"
    while IFS= read -r namespace; do
        [[ -n $namespace ]] || continue
        [[ $namespace = azure-e2e-* ]] ||
            fail "Unknown run-labeled namespace $namespace"
        kubectl --kubeconfig "$KUBECONFIG" delete namespace "$namespace" \
            --wait=true --timeout=10m >/dev/null ||
            fail "Run-owned namespace $namespace did not finish deleting"
    done < <(jq -r '.items[].metadata.name' <<< "$namespaces")
    secrets=$(kubectl --kubeconfig "$KUBECONFIG" --request-timeout=30s \
        -n kube-system get secrets -o json) ||
        fail "Cannot list this cluster's bootstrap tokens"
    jq '{apiVersion:"v1",kind:"List",items:[.items[] |
        select(.type=="bootstrap.kubernetes.io/token") |
        {apiVersion:"v1",kind:"Secret",
         metadata:{namespace:"kube-system",name:.metadata.name}}]}' <<< "$secrets" |
        kubectl --kubeconfig "$KUBECONFIG" delete -f - \
            --ignore-not-found >/dev/null ||
        fail "Could not revoke this cluster's bootstrap tokens"
}
# Scales the large pool to zero during down. Returns non-zero unless the
# pool, and its Nodes when $1 says the API is available, are verified gone.
stop_large_pool() {
    local api_available=$1 exists count
    exists=$(az group exists -n "$WORKERS_RG") || return 1
    [[ $exists = true && -f $PRIVATE_DIR/profile && $(profile) = large ]] || return 0
    count=$(resource_count "$WORKERS_RG" Microsoft.Compute/virtualMachineScaleSets "$PREFIX-l") ||
        return 1
    [[ $count = 1 ]] || return 0
    owned "$(resource_id "$WORKERS_RG" Microsoft.Compute "virtualMachineScaleSets/$PREFIX-l")"
    if [[ $(pool_capacity "$PREFIX-l") != 0 ]]; then
        az vmss scale -g "$WORKERS_RG" -n "$PREFIX-l" \
            --new-capacity 0 --output none ||
            return 1
    fi
    local i remaining
    for ((i=0; i<90; i++)); do
        remaining=unknown
        if [[ $api_available = true && -f $KUBECONFIG &&
            -f $PRIVATE_DIR/cluster-uid ]]; then
            remaining=$(kubectl --kubeconfig "$KUBECONFIG" --request-timeout=30s \
                get nodes -o json | jq --arg pool \
                    "/virtualmachinescalesets/$PREFIX-l/virtualmachines/" \
                    '[.items[]|select(((.spec.providerID//"")|ascii_downcase)|contains($pool))]|length') || {
                remaining=unknown
                api_available=false
            }
        fi
        if [[ $(pool_capacity "$PREFIX-l") = 0 &&
            $(pool_actual "$PREFIX-l") = 0 &&
            $(pool_nics "$PREFIX-l") = 0 ]]; then
            [[ $remaining = 0 ]] && return 0
            [[ $api_available = false ]] && return 1
        fi
        sleep 20
    done
    return 1
}
# Deletes the run's cluster objects, roles, groups and private files, workers
# first, and verifies the cleanup.
down() {
    if [[ $(az group exists -n "$WORKERS_RG") = false &&
        $(az group exists -n "$INFRA_RG") = false ]]; then
        verify_cleanup_inventory
        verify_roles_absent
        local_cleanup
        note "This run's Azure groups and resources are already absent"
        return
    fi
    [[ -f $PRIVATE_DIR/started-at ]] || fail "Cannot delete groups without the start record"
    [[ $(az group exists -n "$WORKERS_RG") = false ]] || owned_group "$WORKERS_RG"
    [[ $(az group exists -n "$INFRA_RG") = false ]] || owned_group "$INFRA_RG"
    local large_verified=true kube_verified=true roles_verified=true infra_exists
    infra_exists=$(az group exists -n "$INFRA_RG") || fail "Cannot check resource group $INFRA_RG"
    if [[ -f $KUBECONFIG && -f $PRIVATE_DIR/cluster-uid && $infra_exists = true ]]; then
        if ! (cleanup_cluster); then
            kube_verified=false
            note "Kubernetes cleanup failed; continue deleting only the owned Azure groups"
        fi
    elif [[ -f $PRIVATE_DIR/cluster-uid && $infra_exists = true ]]; then
        kube_verified=false
        note "Kubeconfig is missing; remove the owned Azure groups but report token cleanup as unverified"
    fi
    if ! stop_large_pool "$kube_verified"; then
        large_verified=false
        note "Large pool did not physically return to zero; delete its owned worker group now"
    fi
    if ! (remove_roles); then
        roles_verified=false
        note "Role removal failed; continue workers-first cleanup and check roles afterward"
    fi
    delete_group "$WORKERS_RG"
    [[ $(az group exists -n "$WORKERS_RG") = false ]] ||
        fail "Workers group must be absent before infra deletion"
    delete_group "$INFRA_RG"
    verify_cleanup_inventory
    if (verify_roles_absent); then
        roles_verified=true
    else
        roles_verified=false
        note "Run-created role IDs remain in private files for another cleanup attempt"
    fi
    [[ $roles_verified = true ]] ||
        fail "Run-created role assignments could not be verified absent"
    local_cleanup
    note "Workers-first cleanup, tunnels, private files and candidate image verified"
    [[ $large_verified = true ]] ||
        fail "Large pool was not independently verified at zero before its group was removed"
    [[ $kube_verified = true ]] ||
        fail "Kubernetes token or namespace cleanup was not verified before its cluster was removed"
}

# Loads env file $1 and runs command $2 with the remaining arguments.
main() {
    case $- in *x*) fail "Disable shell tracing before using the fixture" ;; esac
    if (( $# < 2 )); then
        usage
        exit 2
    fi
    ENV_FILE=$1
    readonly ENV_FILE
    load_env "$ENV_FILE"
    COMMAND=$2
    readonly COMMAND
    shift 2
    case $COMMAND in
        self-check-nsg)
            allowed_nsg_rules
            return ;;
        self-check-calico)
            (( $# == 1 )) || fail "Use self-check-calico with one local manifest"
            check_calico_modes "$1"
            return ;;
        self-check-probe)
            [[ $# = 1 && -d $1 ]] || fail "Use self-check-probe with a private test directory"
            PRIVATE_DIR=$1
            KUBECONFIG=$PRIVATE_DIR/kubeconfig
            RUN_ID=fixture-test
            (trap 'cleanup_network_probe "azure-e2e-fixture-test-network"' EXIT
             probe_http azure-e2e-fixture-test-network probe-a 127.0.0.1 2)
            return ;;
    esac

    validate
    case $COMMAND in
        check) note "Run ID, deadline, Azure subscription and operator /32 are valid" ;;
        image) ensure_image ;;
        infra)
            [[ -s $PRIVATE_DIR/ca-amd64.tar ]] || fail "Run image before creating infra"
            ensure_ssh_key
            if [[ -f $KUBECONFIG && -f $PRIVATE_DIR/cluster-uid ]]; then
                ensure_kubeconfig
                no_test_namespace
            fi
            start_clock
            ensure_group "$INFRA_RG"
            ensure_group "$WORKERS_RG"
            ensure_network
            ensure_roles
            ensure_cp
            ensure_bastion
            tag_created_resources
            note "Infra is ready; start tunnel ssh and tunnel api in separate terminals" ;;
        tunnel) (( $# == 1 )) || fail "Use tunnel ssh or tunnel api"; tunnel "$1" ;;
        cluster) ensure_cluster; tag_created_resources; note "Owned control plane and addons are Ready" ;;
        pools)
            ensure_pools
            priority_classes
            printf 'default\n' > "$PRIVATE_DIR/profile"
            marker allow-kube-system-fixture CA-011
            binding
            tag_created_resources
            note "Default pools, marker and binding are Ready" ;;
        up)
            [[ -s $PRIVATE_DIR/ca-amd64.tar ]] || fail "Run image before creating infra"
            ensure_ssh_key
            if [[ -f $KUBECONFIG && -f $PRIVATE_DIR/cluster-uid ]]; then
                ensure_kubeconfig
                no_test_namespace
            fi
            start_clock
            ensure_group "$INFRA_RG"
            ensure_group "$WORKERS_RG"
            ensure_network
            ensure_roles
            ensure_cp
            ensure_bastion
            [[ -f $PRIVATE_DIR/tunnel-ssh.pid && -f $PRIVATE_DIR/tunnel-api.pid &&
                $(cat "$PRIVATE_DIR/tunnel-ssh.pid") =~ ^[0-9]+$ &&
                $(cat "$PRIVATE_DIR/tunnel-api.pid") =~ ^[0-9]+$ ]] ||
                fail "Start tunnel ssh and tunnel api in attached terminals, then rerun up"
            if ! kill -0 "$(cat "$PRIVATE_DIR/tunnel-ssh.pid")" 2>/dev/null ||
                ! kill -0 "$(cat "$PRIVATE_DIR/tunnel-api.pid")" 2>/dev/null; then
                fail "The run-owned Bastion tunnels stopped"
            fi
            if ! nc -z 127.0.0.1 "$SSH_PORT" || ! nc -z 127.0.0.1 "$API_PORT"; then
                fail "A run-owned Bastion tunnel is not responsive"
            fi
            [[ $(ssh_cp hostname) = "$PREFIX-cp" ]] ||
                fail "The SSH tunnel does not reach this run's control plane"
            ensure_cluster
            ensure_pools
            priority_classes
            printf 'default\n' > "$PRIVATE_DIR/profile"
            marker allow-kube-system-fixture CA-011
            binding
            tag_created_resources
            note "Default fixture is Ready; run ca-deploy then ca-start" ;;
        phase) (( $# == 1 )) || fail "Use phase with one fixture name"; phase "$1" ;;
        ca-deploy) ca_deploy ;;
        ca-start) ca_start ;;
        ca-stop) pause_ca ;;
        dra|disk|signal) (( $# == 1 )) || fail "This command takes one action"; "$COMMAND" "$1" ;;
        run) (( $# == 1 )) || fail "Run one case ID per invocation"; run_case "$1" ;;
        watch) watch ;;
        down) down ;;
        *) usage; fail "Unknown fixture command: $COMMAND" ;;
    esac
}

main "$@"
