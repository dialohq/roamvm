{layout}: {
  apiVersion = "kustomize.config.k8s.io/v1beta1";
  kind = "Kustomization";
  resources = ["config" "warm-volumes.json"];
  images = [
    {
      name = "roamvm";
      newTag = "libvirt-lab";
    }
  ];
  secretGenerator = [
    {
      name = "roamvm-object-store";
      namespace = "default";
      literals = [
        "S3_ENDPOINT=http://${layout.nodes."${layout.cluster}-control-plane".ip}:9000"
        "S3_BUCKET=roamvm"
        "AWS_ACCESS_KEY_ID=roamvm-local"
        "AWS_SECRET_ACCESS_KEY=roamvm-local-test-only"
      ];
    }
  ];
  generatorOptions.disableNameSuffixHash = true;
  patches = [
    {
      target = {
        kind = "Deployment";
        name = "controller";
      };
      patch = builtins.toJSON {
        apiVersion = "apps/v1";
        kind = "Deployment";
        metadata.name = "controller";
        spec.template.spec = {
          nodeSelector."kubernetes.io/hostname" = "${layout.cluster}-control-plane";
          containers = [
            {
              name = "controller";
              env = [
                {
                  name = "RUNNER_IMAGE";
                  value = "roamvm:libvirt-lab";
                }
              ];
            }
          ];
        };
      };
    }
  ];
}
