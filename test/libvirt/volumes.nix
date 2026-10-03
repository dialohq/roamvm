{layout}: {
  apiVersion = "v1";
  kind = "List";
  # Empty, node-local capacity restored with the lab baseline. Binding these
  # needs no provisioning Pod; exhaustion falls back to ordinary local-path.
  # Consumed volumes are deleted, never rebound with another VM's old data.
  items = builtins.concatLists (map
    (node:
      builtins.genList (slot: {
        apiVersion = "v1";
        kind = "PersistentVolume";
        metadata = {
          name = "warm-${node}-${toString slot}";
          labels."roamvm.test/warm-volume" = "true";
          annotations."pv.kubernetes.io/provisioned-by" = "rancher.io/local-path";
        };
        spec = {
          capacity.storage = "64Gi";
          volumeMode = "Filesystem";
          accessModes = ["ReadWriteOnce"];
          storageClassName = "local-path";
          persistentVolumeReclaimPolicy = "Delete";
          hostPath = {
            path = "/var/lib/rancher/k3s/storage/roamvm-warm-${toString slot}";
            type = "DirectoryOrCreate";
          };
          nodeAffinity.required.nodeSelectorTerms = [
            {
              matchExpressions = [
                {
                  key = "kubernetes.io/hostname";
                  operator = "In";
                  values = [node];
                }
              ];
            }
          ];
        };
      })
      8)
    (builtins.filter (node: layout.nodes.${node}.role == "agent") (builtins.attrNames layout.nodes)));
}
