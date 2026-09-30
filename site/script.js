const stages = {
  start: {
    label: "01 / START",
    title: "LET THE SCHEDULER CHOOSE.",
    copy: "Kubernetes places the runner Pod. The runtime mounts the OCI base, restores the saved changes, and boots the VM on node A.",
    command: "roamvm start devbox",
    note: "A normal Pod owns placement, resource requests, and networking.",
    checkpoint: "Latest saved disk, ready to restore.",
    activeNode: "a",
  },
  stop: {
    label: "02 / STOP",
    title: "SAVE THE DISK. RELEASE THE NODE.",
    copy: "The guest shuts down. RoamVM uploads and verifies its changed disk, commits the replacement checkpoint, then releases the Pod and working PVC.",
    command: "roamvm stop devbox",
    note: "Stopped means durably saved. The previous checkpoint is removed only after its replacement is committed.",
    checkpoint: "New checkpoint committed. Previous copy removed.",
    activeNode: null,
  },
  move: {
    label: "03 / MOVE",
    title: "SAME DISK. DIFFERENT NODE.",
    copy: "Start again. If Kubernetes selects node B, the runtime restores your saved disk there. Your files, packages, and settings come with you.",
    command: "roamvm start devbox",
    note: "To test a move, cordon node A after a durable stop. No direct node-to-node disk copy is needed.",
    checkpoint: "Saved disk restored on node B.",
    activeNode: "b",
  },
};

const panel = document.querySelector("#lifecycle-panel");
const stageButtons = document.querySelectorAll("[data-stage]");
document.querySelector(".lifecycle-controls").hidden = false;
document.querySelector("#copy-example").hidden = false;

for (const button of stageButtons) {
  button.addEventListener("click", () => {
    const stage = stages[button.dataset.stage];
    panel.dataset.currentStage = button.dataset.stage;
    for (const candidate of stageButtons) {
      candidate.setAttribute("aria-pressed", String(candidate === button));
    }
    for (const field of ["label", "title", "copy", "command", "note"]) {
      document.querySelector(`#stage-${field}`).textContent = stage[field];
    }
    document.querySelector("#checkpoint-label").textContent = stage.checkpoint;
    for (const node of document.querySelectorAll("[data-node]")) {
      const running = node.dataset.node === stage.activeNode;
      node.classList.toggle("is-running", running);
      node.querySelector(".node-vm").textContent = running ? "VM" : "—";
      node.querySelector(".node-state").textContent = running
        ? "RUNNING"
        : "AVAILABLE";
      if (running) {
        const light = document.createElement("span");
        light.className = "node-light";
        node.querySelector(".node-vm").append(light);
      }
    }
  });
}

document.querySelector("#copy-example").addEventListener("click", async () => {
  const example = document.querySelector("#vm-example");
  const status = document.querySelector("#copy-status");
  try {
    await navigator.clipboard.writeText(example.textContent);
    status.textContent =
      "Copied. Replace the image reference with your digest.";
  } catch {
    const range = document.createRange();
    range.selectNodeContents(example);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    status.textContent = "Select and copy the YAML using your browser.";
  }
});
