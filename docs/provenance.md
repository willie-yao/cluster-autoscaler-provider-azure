# Source provenance

Imported selected files unchanged from `kubernetes/autoscaler@7df904eabeef796b63ca845b7d5e7f6d9a300f44`.
The first commit preserves upstream paths and modes. The second only moves files.
The third changes only the module and owned package paths, with Go formatting.

Run `hack/verify-upstream.sh` with Git, Python 3, Go 1.26 and network access.
It verifies the first three commits and reports later commits separately.
It does not claim later application changes are identical to upstream.

<!-- import-manifest
{
  "upstream": "7df904eabeef796b63ca845b7d5e7f6d9a300f44",
  "url": "https://github.com/kubernetes/autoscaler.git",
  "files": [
    {
      "source": ".devcontainer/devcontainer.json",
      "target": ".devcontainer/devcontainer.json",
      "mode": "100644",
      "blob": "d7a66a7b6e05cea0e3453554df5ddf9e3d925aee"
    },
    {
      "source": "cluster-autoscaler/.dockerignore",
      "target": ".dockerignore",
      "mode": "100644",
      "blob": "e19df105d06f911cf1335cd1c8f27ff5d92a12b9"
    },
    {
      "source": ".github/ISSUE_TEMPLATE/bug_report.md",
      "target": ".github/ISSUE_TEMPLATE/bug_report.md",
      "mode": "100644",
      "blob": "b900ab4bf48faeecd0be1b835eba536d6a17dd29"
    },
    {
      "source": ".github/ISSUE_TEMPLATE/feature_request.md",
      "target": ".github/ISSUE_TEMPLATE/feature_request.md",
      "mode": "100644",
      "blob": "ff9fccf102d95a7f36b11883ac76f4f6ff90412c"
    },
    {
      "source": ".github/PULL_REQUEST_TEMPLATE.md",
      "target": ".github/PULL_REQUEST_TEMPLATE.md",
      "mode": "100644",
      "blob": "1bc58284a806e6f0f53df82cf55ce05f6a1a5ff9"
    },
    {
      "source": ".github/dependabot.yml",
      "target": ".github/dependabot.yml",
      "mode": "100644",
      "blob": "f6ba08827944cf5504b1906315051e69d4ec4a66"
    },
    {
      "source": ".github/kind-config.yaml",
      "target": ".github/kind-config.yaml",
      "mode": "100644",
      "blob": "a7ed478f85cd691e66cad863bd39929e14a88f0c"
    },
    {
      "source": ".github/workflows/ca-test.yaml",
      "target": ".github/workflows/ca-test.yaml",
      "mode": "100644",
      "blob": "3324f40d3a494758c7b63b2acaffc91dd2c723c6"
    },
    {
      "source": ".github/workflows/pr.yaml",
      "target": ".github/workflows/pr.yaml",
      "mode": "100644",
      "blob": "9cc4f0d99ff04ba71802e9363fb2198742a8e870"
    },
    {
      "source": ".github/workflows/verify.yaml",
      "target": ".github/workflows/verify.yaml",
      "mode": "100644",
      "blob": "bf120bfcaba26d375a29395da9efe4e36a57014c"
    },
    {
      "source": "cluster-autoscaler/.gitignore",
      "target": ".gitignore",
      "mode": "100644",
      "blob": "3a41fc81f75bf87e8e142cbfd26fbe77742427c1"
    },
    {
      "source": ".pre-commit-config.yaml",
      "target": ".pre-commit-config.yaml",
      "mode": "100644",
      "blob": "eb6183a7e2ac208a87940ca1d218ec723ac59b67"
    },
    {
      "source": "CONTRIBUTING.md",
      "target": "CONTRIBUTING.md",
      "mode": "100644",
      "blob": "1b3554cc98a461d94f9360898b2b08deb6b5b7d0"
    },
    {
      "source": "cluster-autoscaler/Dockerfile",
      "target": "Dockerfile",
      "mode": "100644",
      "blob": "59ebeef9a7f446cb1ab683095dcc9ea7f8918829"
    },
    {
      "source": "LICENSE",
      "target": "LICENSE",
      "mode": "100644",
      "blob": "d645695673349e3947e8e5ae42332d0ac3164cd7"
    },
    {
      "source": "cluster-autoscaler/Makefile",
      "target": "Makefile",
      "mode": "100644",
      "blob": "2cb3ab0c9e62b8ef3334dfead6e168d7ef0004e8"
    },
    {
      "source": "cluster-autoscaler/README.md",
      "target": "README.md",
      "mode": "100644",
      "blob": "e139235f5d7e37d7f0475467fbde3edadeed74c1"
    },
    {
      "source": "cluster-autoscaler/charts/OWNERS",
      "target": "charts/OWNERS",
      "mode": "100644",
      "blob": "76b1b34db994ebe6881016d63367d28d817509ed"
    },
    {
      "source": "cluster-autoscaler/charts/README.md",
      "target": "charts/README.md",
      "mode": "100644",
      "blob": "7d88ad31e5954795b3f48a81139839c83461a224"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/.helmignore",
      "target": "charts/cluster-autoscaler/.helmignore",
      "mode": "100644",
      "blob": "0e8a0eb36f4ca2c939201c0d54b5d82a1ea34778"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/Chart.yaml",
      "target": "charts/cluster-autoscaler/Chart.yaml",
      "mode": "100644",
      "blob": "2d14e3b02669e4c483134196c8808b37af5a3c86"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/README.md",
      "target": "charts/cluster-autoscaler/README.md",
      "mode": "100644",
      "blob": "29305b7eafbb92262ecf4f244f2fb69c96871472"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/README.md.gotmpl",
      "target": "charts/cluster-autoscaler/README.md.gotmpl",
      "mode": "100644",
      "blob": "957658ce1a365a86292a1f31077901fa312d9c56"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/NOTES.txt",
      "target": "charts/cluster-autoscaler/templates/NOTES.txt",
      "mode": "100644",
      "blob": "1a87a3d10b502bcef8cd73350d4926bb8eba6de6"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/_helpers.tpl",
      "target": "charts/cluster-autoscaler/templates/_helpers.tpl",
      "mode": "100644",
      "blob": "c7e80f4d8e4c272bc95d481c82c6b0d109de2f22"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/clusterrole.yaml",
      "target": "charts/cluster-autoscaler/templates/clusterrole.yaml",
      "mode": "100644",
      "blob": "445724cff878299352cfc452f51dd215b47d4f56"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/clusterrolebinding.yaml",
      "target": "charts/cluster-autoscaler/templates/clusterrolebinding.yaml",
      "mode": "100644",
      "blob": "59e6ef67d60538639b83d21619cc02d753fc32a2"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/configmap.yaml",
      "target": "charts/cluster-autoscaler/templates/configmap.yaml",
      "mode": "100644",
      "blob": "6cd0c4064bfa366de98052f85fc6d554983e8046"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/deployment.yaml",
      "target": "charts/cluster-autoscaler/templates/deployment.yaml",
      "mode": "100644",
      "blob": "f66987e06414f64cc8cbfb5961ac81f6dc25b920"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/extra-manifests.yaml",
      "target": "charts/cluster-autoscaler/templates/extra-manifests.yaml",
      "mode": "100644",
      "blob": "a9bb3b6ba8ef1e7de8ea61832973a6af5a1c1a10"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/pdb.yaml",
      "target": "charts/cluster-autoscaler/templates/pdb.yaml",
      "mode": "100644",
      "blob": "0f8a69ecd6ca6f2d11028b44bbd1d5f3b8e6fa84"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/podsecuritypolicy.yaml",
      "target": "charts/cluster-autoscaler/templates/podsecuritypolicy.yaml",
      "mode": "100644",
      "blob": "e3ce59973686e3ef12b909a437b484211efedcf8"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/priority-expander-configmap.yaml",
      "target": "charts/cluster-autoscaler/templates/priority-expander-configmap.yaml",
      "mode": "100644",
      "blob": "8259f14ff4b2c47ef36da014c65ed3be1dbdce40"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/prometheusrule.yaml",
      "target": "charts/cluster-autoscaler/templates/prometheusrule.yaml",
      "mode": "100644",
      "blob": "097c969ef93184ce7089747e453615672f09507c"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/role.yaml",
      "target": "charts/cluster-autoscaler/templates/role.yaml",
      "mode": "100644",
      "blob": "80cf30a11022d2aa5c9f7f02dff0122ac631a68d"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/rolebinding.yaml",
      "target": "charts/cluster-autoscaler/templates/rolebinding.yaml",
      "mode": "100644",
      "blob": "9436aabe641b70ddb75cb553cfc93ec9d7ce7a49"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/secret.yaml",
      "target": "charts/cluster-autoscaler/templates/secret.yaml",
      "mode": "100644",
      "blob": "760cc3c5a772c50db87aa9de02fb8903b4b75b2f"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/service.yaml",
      "target": "charts/cluster-autoscaler/templates/service.yaml",
      "mode": "100644",
      "blob": "c8bd40795d25d5879e59aaaee7fcf9a129a74c08"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/serviceaccount.yaml",
      "target": "charts/cluster-autoscaler/templates/serviceaccount.yaml",
      "mode": "100644",
      "blob": "465b5aad202792f7f182b12def8cb427dc46ec25"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/servicemonitor.yaml",
      "target": "charts/cluster-autoscaler/templates/servicemonitor.yaml",
      "mode": "100644",
      "blob": "9ce83a2ef6e1b23ce640cd0ef605869cc4756935"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/templates/vpa.yaml",
      "target": "charts/cluster-autoscaler/templates/vpa.yaml",
      "mode": "100644",
      "blob": "560dab00a4de2dd924743a5e9301cd57dca305a5"
    },
    {
      "source": "cluster-autoscaler/charts/cluster-autoscaler/values.yaml",
      "target": "charts/cluster-autoscaler/values.yaml",
      "mode": "100644",
      "blob": "24cfaca4f14494ef6be283434b464538c16a7831"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/OWNERS",
      "target": "pkg/cloudprovider/azure/OWNERS",
      "mode": "100644",
      "blob": "d9c7e2cc838f6aae3f633f76c646d8398c35f690"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/README.md",
      "target": "pkg/cloudprovider/azure/README.md",
      "mode": "100644",
      "blob": "1e5c5956b7c40582fa81e096a218b17ccaaf897b"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_agent_pool.go",
      "target": "pkg/cloudprovider/azure/azure_agent_pool.go",
      "mode": "100644",
      "blob": "915d8615f09dad96c93e5cc1f3193456c0f1c451"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_agent_pool_test.go",
      "target": "pkg/cloudprovider/azure/azure_agent_pool_test.go",
      "mode": "100644",
      "blob": "b02a7c1e22574930b1960eefeb872e3f3be5c188"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_autodiscovery.go",
      "target": "pkg/cloudprovider/azure/azure_autodiscovery.go",
      "mode": "100644",
      "blob": "b8232b50c8c3f21f915c1b3071b22567066d9f25"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_autodiscovery_test.go",
      "target": "pkg/cloudprovider/azure/azure_autodiscovery_test.go",
      "mode": "100644",
      "blob": "d83abd3b038100fb2bb83af203e0495531695a95"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_cache.go",
      "target": "pkg/cloudprovider/azure/azure_cache.go",
      "mode": "100644",
      "blob": "3069ea622c6dc2a5767e2644c704119cbe373b9f"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_cache_test.go",
      "target": "pkg/cloudprovider/azure/azure_cache_test.go",
      "mode": "100644",
      "blob": "8b8a66d3f8f6c7ac0cff4f6ce030267db7dec5b3"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_client.go",
      "target": "pkg/cloudprovider/azure/azure_client.go",
      "mode": "100644",
      "blob": "9ded740b8200ef5f18848e9ece6251b7e7a68f2d"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_client_test.go",
      "target": "pkg/cloudprovider/azure/azure_client_test.go",
      "mode": "100644",
      "blob": "2490a236976c64c0f8b11d720a94f118859083b1"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_cloud_provider.go",
      "target": "pkg/cloudprovider/azure/azure_cloud_provider.go",
      "mode": "100644",
      "blob": "ba431d9642c53f721b22fe3cff2b810c9052de8e"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_cloud_provider_test.go",
      "target": "pkg/cloudprovider/azure/azure_cloud_provider_test.go",
      "mode": "100644",
      "blob": "8a58abb521dbf30b38b7a37b0e0c01c72fa46101"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_config.go",
      "target": "pkg/cloudprovider/azure/azure_config.go",
      "mode": "100644",
      "blob": "06b927c9c68e438adcd08e277c722ce7a8144e9c"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_config_test.go",
      "target": "pkg/cloudprovider/azure/azure_config_test.go",
      "mode": "100644",
      "blob": "6fe6e112cfd6583be28534e0ba8328f855bac20d"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_deployment_client.go",
      "target": "pkg/cloudprovider/azure/azure_deployment_client.go",
      "mode": "100644",
      "blob": "9ca9ec85885eece6571d9f18f661e8dd91956f88"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_fakes.go",
      "target": "pkg/cloudprovider/azure/azure_fakes.go",
      "mode": "100644",
      "blob": "21a1981c749e9e3390a0a1320191469e41f92bad"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_force_delete_scale_set.go",
      "target": "pkg/cloudprovider/azure/azure_force_delete_scale_set.go",
      "mode": "100644",
      "blob": "4c35730a9be89ef4fe4567f2bfa312e5057ae61a"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_force_delete_scale_set_test.go",
      "target": "pkg/cloudprovider/azure/azure_force_delete_scale_set_test.go",
      "mode": "100644",
      "blob": "f5586e105ec9404f5ca40cadfbe9f5ad849194bb"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_instance.go",
      "target": "pkg/cloudprovider/azure/azure_instance.go",
      "mode": "100644",
      "blob": "d771e0b4dac9156cecfa293fa931993d81c0c8c6"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_instance_gpu_sku.go",
      "target": "pkg/cloudprovider/azure/azure_instance_gpu_sku.go",
      "mode": "100644",
      "blob": "2426ad4bc132473816bf1ebffb71257ba78b1944"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_instance_types.go",
      "target": "pkg/cloudprovider/azure/azure_instance_types.go",
      "mode": "100644",
      "blob": "0dec7c8bcb4b6235d09aa59735946b35bc4e2be2"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_instance_types/gen.go",
      "target": "pkg/cloudprovider/azure/azure_instance_types/gen.go",
      "mode": "100644",
      "blob": "a9ce2d79f98f4ceda3a810fb0a8c25eb4d1deab1"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_manager.go",
      "target": "pkg/cloudprovider/azure/azure_manager.go",
      "mode": "100644",
      "blob": "6bfe2c461c2707cf2124cb4ba08ec87b6c740567"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_manager_test.go",
      "target": "pkg/cloudprovider/azure/azure_manager_test.go",
      "mode": "100644",
      "blob": "2a0d22a9d44e0784a4d512fae8f42fa1d9bfea5a"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_mock_agentpool_client.go",
      "target": "pkg/cloudprovider/azure/azure_mock_agentpool_client.go",
      "mode": "100644",
      "blob": "d408ee67b928f7c8f5a90adc629cd20af95031d6"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_mock_storageaccount_client_test.go",
      "target": "pkg/cloudprovider/azure/azure_mock_storageaccount_client_test.go",
      "mode": "100644",
      "blob": "5df69549d50b42210b87434788770a3484b5177d"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_mock_virtualmachine_client_test.go",
      "target": "pkg/cloudprovider/azure/azure_mock_virtualmachine_client_test.go",
      "mode": "100644",
      "blob": "00ad8f4431e27cb058ee9345a072876a4ca7f5ae"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_mock_vmss_delete_client_test.go",
      "target": "pkg/cloudprovider/azure/azure_mock_vmss_delete_client_test.go",
      "mode": "100644",
      "blob": "0e567a77214600405c250a8c06543ac0e955cc68"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_scale_set.go",
      "target": "pkg/cloudprovider/azure/azure_scale_set.go",
      "mode": "100644",
      "blob": "31e21830aff2b8e1fe5e30f991cb818cf3b38bb4"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_scale_set_instance_cache.go",
      "target": "pkg/cloudprovider/azure/azure_scale_set_instance_cache.go",
      "mode": "100644",
      "blob": "24243711dddcb8cf2b23912fa4388aed6b3f924b"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_scale_set_instance_cache_test.go",
      "target": "pkg/cloudprovider/azure/azure_scale_set_instance_cache_test.go",
      "mode": "100644",
      "blob": "2c4df15ea3e1da678bcc5ed8f3d649cd5cbdfae9"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_scale_set_test.go",
      "target": "pkg/cloudprovider/azure/azure_scale_set_test.go",
      "mode": "100644",
      "blob": "3ffb595b0afe1328fea4be171c3103b352b6f910"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_template.go",
      "target": "pkg/cloudprovider/azure/azure_template.go",
      "mode": "100644",
      "blob": "383400138968f33891709ce6c1886e810f4cb009"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_template_test.go",
      "target": "pkg/cloudprovider/azure/azure_template_test.go",
      "mode": "100644",
      "blob": "a1fd81f8d95c71a826a82181c7523b0ab9c0fb50"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_util.go",
      "target": "pkg/cloudprovider/azure/azure_util.go",
      "mode": "100644",
      "blob": "aaa8a4e8989114ef0270e96aedf604ac3a6c3a15"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_util_test.go",
      "target": "pkg/cloudprovider/azure/azure_util_test.go",
      "mode": "100644",
      "blob": "c9b4ae1cb1f8d5638037d25ced7cf28308bba023"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_vms_pool.go",
      "target": "pkg/cloudprovider/azure/azure_vms_pool.go",
      "mode": "100644",
      "blob": "21327e08122040de1317150a3f3bbae2a3eadd71"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_vms_pool_test.go",
      "target": "pkg/cloudprovider/azure/azure_vms_pool_test.go",
      "mode": "100644",
      "blob": "c482714e0adf78896c39a96b947673e0402f64ef"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/azure_vmss_delete_client.go",
      "target": "pkg/cloudprovider/azure/azure_vmss_delete_client.go",
      "mode": "100644",
      "blob": "664c6685bb1f8b1911ce7950f63ad99220c2da53"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-aks.yaml",
      "target": "deploy/cluster-autoscaler-aks.yaml",
      "mode": "100644",
      "blob": "114ece6b58ce5e280ce2dca4fe45e237c256dd1a"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-autodiscover.yaml",
      "target": "deploy/cluster-autoscaler-autodiscover.yaml",
      "mode": "100644",
      "blob": "9808af1ba74adeb195496b5791c3a23e7d1b99f8"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-standard-control-plane.yaml",
      "target": "deploy/cluster-autoscaler-standard-control-plane.yaml",
      "mode": "100644",
      "blob": "a3496d20edb83b057a43abaccee27d72813bbf40"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-standard-msi.yaml",
      "target": "deploy/cluster-autoscaler-standard-msi.yaml",
      "mode": "100644",
      "blob": "b585907471feb92fc61de2cf077af19dd4062e20"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-standard.yaml",
      "target": "deploy/cluster-autoscaler-standard.yaml",
      "mode": "100644",
      "blob": "eb36e80c7b40ed1efa253c2364d5f8b82eea51ad"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-vmss-control-plane.yaml",
      "target": "deploy/cluster-autoscaler-vmss-control-plane.yaml",
      "mode": "100644",
      "blob": "942a90154e073a45c0fc3e14b369cf17448b2d79"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-vmss-msi.yaml",
      "target": "deploy/cluster-autoscaler-vmss-msi.yaml",
      "mode": "100644",
      "blob": "fce4ef38278a25f178e48fc54b297a561a57563b"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/cluster-autoscaler-vmss.yaml",
      "target": "deploy/cluster-autoscaler-vmss.yaml",
      "mode": "100644",
      "blob": "f4ea7980fde0e0b07d04b7b41128f593d3523227"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/dev/README.md",
      "target": "deploy/dev/README.md",
      "mode": "100644",
      "blob": "9d96781910570f2e423c26402c7f288ffa865da3"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/dev/aks-dev-deploy.sh",
      "target": "deploy/dev/aks-dev-deploy.sh",
      "mode": "100755",
      "blob": "f66cec60216329401ce72249b6cae9bb1240255d"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/dev/aks-dev.bicep",
      "target": "deploy/dev/aks-dev.bicep",
      "mode": "100644",
      "blob": "0998e2725070bb771b5f7e2a76a77664ea98e365"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/dev/cluster-autoscaler-vmss-wi-dynamic.yaml.tpl",
      "target": "deploy/dev/cluster-autoscaler-vmss-wi-dynamic.yaml.tpl",
      "mode": "100644",
      "blob": "277f2cfee0a09f4fcf0124c1f0c967b562cb7ba6"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/dev/skaffold.yaml",
      "target": "deploy/dev/skaffold.yaml",
      "mode": "100644",
      "blob": "d8a4f6d2b13565c85d5f8e5bdfc6615ef1a42015"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/examples/workloads/inflate.yaml",
      "target": "deploy/workloads/inflate.yaml",
      "mode": "100644",
      "blob": "6b6c20d0f04b33754ace39c94fbbf20c1500f320"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/fake_poller_test.go",
      "target": "pkg/cloudprovider/azure/fake_poller_test.go",
      "mode": "100644",
      "blob": "98309efebc45d28f21f77a621970fc30ce006b33"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/.gitignore",
      "target": "test/.gitignore",
      "mode": "100644",
      "blob": "9c2b30a747bf1effba383d0702ea700af79fbb81"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/Makefile",
      "target": "test/Makefile",
      "mode": "100644",
      "blob": "74672f039f18daea59827546f403b6f4690670b6"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/go.mod",
      "target": "test/go.mod",
      "mode": "100644",
      "blob": "6f5bb8cdcbb5a5050e489f2aa191bf7eada76312"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/go.sum",
      "target": "test/go.sum",
      "mode": "100644",
      "blob": "259786c5f79e77720c0edf2c0107cb986c63949f"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/pkg/environment/environment.go",
      "target": "test/pkg/environment/environment.go",
      "mode": "100644",
      "blob": "6e3551f4664c48811da85ba7dbd98c3cbcfe389d"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/suites/scaleup/suite_test.go",
      "target": "test/suites/scaleup/suite_test.go",
      "mode": "100644",
      "blob": "cbf1c67fff84a31b9d57aaf766611a9c897b42ce"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/test/templates/cluster-template-prow-aks-aso-cluster-autoscaler.yaml",
      "target": "test/templates/cluster-template-prow-aks-aso-cluster-autoscaler.yaml",
      "mode": "100644",
      "blob": "7be9c3c79a3b62f815bce716242e23722d67565c"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/testdata/test.pfx",
      "target": "pkg/cloudprovider/azure/testdata/test.pfx",
      "mode": "100644",
      "blob": "693133363fbdbc636c1d11671a9d6e28fded44a9"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/azure/testdata/testnopassword.pfx",
      "target": "pkg/cloudprovider/azure/testdata/testnopassword.pfx",
      "mode": "100644",
      "blob": "0b32730ed8c7ee2997406c5d12ab94dba7dde8c4"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/router/README.md",
      "target": "pkg/cloudprovider/router/README.md",
      "mode": "100644",
      "blob": "edcdfe2f19d3b92b3ba7572b61aebc2b06e58ccc"
    },
    {
      "source": "cluster-autoscaler/cloudprovider/router/router_azure.go",
      "target": "pkg/cloudprovider/router/router_azure.go",
      "mode": "100644",
      "blob": "a2338fd061866687f423c67dd9023b6bfcc5945d"
    },
    {
      "source": "code-of-conduct.md",
      "target": "code-of-conduct.md",
      "mode": "100644",
      "blob": "0d15c00cf32529a51f1db0697ab8b0b0669fdc13"
    },
    {
      "source": "cluster-autoscaler/go.mod",
      "target": "go.mod",
      "mode": "100644",
      "blob": "0f520d3ad538501d78564f9c3d4f7874ef7e43c2"
    },
    {
      "source": "cluster-autoscaler/go.sum",
      "target": "go.sum",
      "mode": "100644",
      "blob": "b51d6718cd6e831d7d2ad3ee04576f45b23280eb"
    },
    {
      "source": "hack/OWNERS",
      "target": "hack/OWNERS",
      "mode": "100644",
      "blob": "641cd4a346ad5f7a75717f5a5ee7a7e59a6ffc9b"
    },
    {
      "source": "hack/boilerplate/boilerplate.generatego.txt",
      "target": "hack/boilerplate/boilerplate.generatego.txt",
      "mode": "100644",
      "blob": "0926592d38950da8795faa107a4804222580cb7b"
    },
    {
      "source": "hack/boilerplate/boilerplate.go.txt",
      "target": "hack/boilerplate/boilerplate.go.txt",
      "mode": "100644",
      "blob": "b7c650da47014ddf7e297b1724c922472a6dd8d3"
    },
    {
      "source": "hack/boilerplate/boilerplate.py",
      "target": "hack/boilerplate/boilerplate.py",
      "mode": "100755",
      "blob": "882f2a5d392968c15d7b5dd95993efb00516a1ed"
    },
    {
      "source": "hack/boilerplate/boilerplate.py.txt",
      "target": "hack/boilerplate/boilerplate.py.txt",
      "mode": "100644",
      "blob": "6118b2faf27903a88e94d190857746d7ac66eaea"
    },
    {
      "source": "hack/boilerplate/boilerplate.sh.txt",
      "target": "hack/boilerplate/boilerplate.sh.txt",
      "mode": "100644",
      "blob": "069e282bc855a411736779a3272fb5e5be628c1f"
    },
    {
      "source": "hack/install-verify-tools.sh",
      "target": "hack/install-verify-tools.sh",
      "mode": "100755",
      "blob": "68f8a47f889ff2cd447430c974a11314dbe88f36"
    },
    {
      "source": "hack/kube-env.sh",
      "target": "hack/kube-env.sh",
      "mode": "100644",
      "blob": "4415df155f99ffeca6fded9d4a565639e1727275"
    },
    {
      "source": "cluster-autoscaler/hack/list-owners.py",
      "target": "hack/list-owners.py",
      "mode": "100644",
      "blob": "7b37993b582519ef7c747d63e7955851debb605b"
    },
    {
      "source": "hack/scripts/ca_metrics_parser.py",
      "target": "hack/scripts/ca_metrics_parser.py",
      "mode": "100755",
      "blob": "51083c0d74ec7638ecd38e91b9ba913b18b78fdc"
    },
    {
      "source": "cluster-autoscaler/hack/submodule-k8s.sh",
      "target": "hack/submodule-k8s.sh",
      "mode": "100755",
      "blob": "2e409348fa565377dc4d3489635c9d449cedf8ea"
    },
    {
      "source": "cluster-autoscaler/hack/update-deps.sh",
      "target": "hack/update-deps.sh",
      "mode": "100755",
      "blob": "53a94a08250e826490091dc5b7397867684778c3"
    },
    {
      "source": "hack/update-gofmt.sh",
      "target": "hack/update-gofmt.sh",
      "mode": "100755",
      "blob": "09a4c616ee316841b83b89c3ed25918563ac3c60"
    },
    {
      "source": "hack/verify-all.sh",
      "target": "hack/verify-all.sh",
      "mode": "100755",
      "blob": "bda2aba3b9c28d61def8ae35c56a297c438609a6"
    },
    {
      "source": "hack/verify-boilerplate.sh",
      "target": "hack/verify-boilerplate.sh",
      "mode": "100755",
      "blob": "66f85df7a614a65a79c9c3c8ac8ba74bf5fe2725"
    },
    {
      "source": "hack/verify-gofmt.sh",
      "target": "hack/verify-gofmt.sh",
      "mode": "100755",
      "blob": "9f79f3cd1b1622fd943d25cfc4f748fe714c90ae"
    },
    {
      "source": "hack/verify-golint.sh",
      "target": "hack/verify-golint.sh",
      "mode": "100755",
      "blob": "a5b7fbf7d279c6cc90b31e8127b42785c278945f"
    },
    {
      "source": "hack/verify-spelling.sh",
      "target": "hack/verify-spelling.sh",
      "mode": "100755",
      "blob": "c3142a6f54dbdf165bdcc8086b545e57ad183678"
    },
    {
      "source": "cluster-autoscaler/main.go",
      "target": "main.go",
      "mode": "100644",
      "blob": "9d80f1ba2f6b38bafe906f4db6ce4a2e811772b8"
    },
    {
      "source": "cluster-autoscaler/version/version.go",
      "target": "pkg/version/version.go",
      "mode": "100644",
      "blob": "8ccae3ee4e2b5e069fd8cc1ab2b9a38dcaa74d1b"
    }
  ],
  "substitutions": [
    [
      "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test",
      "github.com/Azure/cluster-autoscaler-provider-azure/test"
    ],
    [
      "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure",
      "github.com/Azure/cluster-autoscaler-provider-azure/pkg/cloudprovider/azure"
    ],
    [
      "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/router",
      "github.com/Azure/cluster-autoscaler-provider-azure/pkg/cloudprovider/router"
    ],
    [
      "k8s.io/autoscaler/cluster-autoscaler/version",
      "github.com/Azure/cluster-autoscaler-provider-azure/pkg/version"
    ],
    [
      "module k8s.io/autoscaler/cluster-autoscaler\n",
      "module github.com/Azure/cluster-autoscaler-provider-azure\n"
    ],
    [
      "k8s.io/autoscaler/cluster-autoscaler/go.mod",
      "github.com/Azure/cluster-autoscaler-provider-azure/go.mod"
    ]
  ]
}
-->
