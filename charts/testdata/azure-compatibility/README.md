# Saved upstream chart renders

The `*.upstream.yaml` files are unmodified Helm renders of `kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`, from `cluster-autoscaler/charts/cluster-autoscaler`. They were produced with Helm `v4.0.4`, release `migration`, namespace `kube-system`, and `--kube-version 1.37.0-rc.1`. The `--kube-version` value only sets the version that `helm template` renders for. It does not mean the chart was tested on that Kubernetes version.

All credentials are synthetic. `common.yaml` selects Azure and supplies the same explicit image to both charts. The repository's default image is a placeholder, which is an intended difference.

| Fixture | Coverage |
| --- | --- |
| `explicit-sp` | Two explicit VMSS groups, including min zero, generated service-principal Secret |
| `discovery-managed` | Templated cluster discovery name, managed identity, generated Secret |
| `explicit-existing-secret` | Existing Secrets, named Secret volume and items, config-file argument, extra environment, priority expander, ConfigMap lock, names and labels |
| `discovery-managed-existing` | Managed identity with an existing Secret |
| `discovery-workload-vpa` | Workload identity labels and service account, VPA policy and recommender |
| `explicit-namespaced-rbac` | Namespaced RBAC and additional rules |

`make test-chart` runs strict Helm lint and compares complete parsed resources. Only YAML formatting, mapping order, and document order are irrelevant. Lists, arguments, environment, selectors, RBAC, Secrets, and volume content are compared. Every fixture must have exactly one Deployment. VPA is required in its enabled fixture and absent from the others through whole-resource comparison.

There are two field-level exceptions. Upstream's managed-identity `ARM_USER_ASSIGNED_IDENTITY_ID` reference ignores `secretKeyRefNameOverride`. The test first asserts upstream's exact value and then expects the corrected Secret name. The chart version bump from `9.59.0` to `9.59.1` changes `metadata.labels["helm.sh/chart"]` from `cluster-autoscaler-9.59.0` to `cluster-autoscaler-9.59.1`. The test checks the saved upstream label and then changes only that label in the saved upstream resources. Selectors, pod labels, and every other resource field are still compared.

## Check the saved renders against upstream

From the repository root, with the pinned upstream Git object available:

```sh
reference=$(mktemp -d)
git archive c85f5afed954f7ecdbfb9091da2202426806d8d8 \
  cluster-autoscaler/charts/cluster-autoscaler | tar -xf - -C "$reference"
for values in charts/testdata/azure-compatibility/*.values.yaml; do
  helm template migration \
    "$reference/cluster-autoscaler/charts/cluster-autoscaler" \
    --namespace kube-system --kube-version 1.37.0-rc.1 \
    -f charts/testdata/azure-compatibility/common.yaml -f "$values" \
    > "$reference/rendered.yaml"
  diff -u "${values%.values.yaml}.upstream.yaml" "$reference/rendered.yaml"
done
shasum -a 256 -c charts/testdata/azure-compatibility/SHA256SUMS
```

The reference directory is temporary and can be removed after inspection. If Git does not contain the pin, obtain that exact upstream source separately. Do not overwrite the saved renders with local chart output, or refresh them only to make a failing comparison pass.
