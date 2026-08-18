const POLL_INTERVAL_MS = 4000;

const cy = cytoscape({
  container: document.getElementById("graph"),
  style: [
    { selector: "node", style: {
        "background-color": "#6b7684",
        "label": "data(displayLabel)",
        "color": "#dfe6ee",
        "font-size": 10,
        "text-valign": "bottom",
        "text-margin-y": 4,
        "text-outline-width": 2,
        "text-outline-color": "#0f1720",
        "width": 26, "height": 26,
        "border-width": 2, "border-color": "#0f1720",
      } },
    { selector: "node[?srCapable]", style: { "background-color": "#5a8dee" } },
    { selector: "node[?isSession]", style: { "background-color": "#ff8a3d", "border-color": "#ffb37a", "width": 34, "height": 34 } },
    { selector: "node.highlighted", style: { "border-color": "#ff8a3d", "border-width": 4 } },
    { selector: "node.search-match", style: { "border-color": "#c9e64d", "border-width": 4 } },
    { selector: "node.dimmed", style: { "opacity": 0.25 } },
    { selector: "edge", style: {
        "width": 2, "line-color": "#3a4657", "curve-style": "bezier",
        "target-arrow-shape": "none", "opacity": 0.9,
      } },
    { selector: "edge.highlighted", style: {
        "line-color": "#ff8a3d", "width": 5, "z-index": 10,
      } },
    { selector: "edge.dimmed", style: { "opacity": 0.12 } },
  ],
});

let latestPolicies = [];
let latestNodesById = {};
let selectedNodeId = null;
let activePolicyName = null;
let layoutRan = false;
let searchQuery = "";

function nodeDisplayLabel(n) {
  const sid = n.sid !== null && n.sid !== undefined ? ` (${n.sid})` : "";
  return `${n.label}${sid}`;
}

function edgeId(a, b) {
  return [a, b].sort().join("__");
}

async function poll() {
  let resp;
  try {
    resp = await fetch("/api/state");
  } catch (e) {
    document.getElementById("pollError").textContent = `fetch failed: ${e}`;
    return;
  }
  const data = await resp.json();

  document.getElementById("pollError").textContent = data.error ? `poll error: ${data.error}` : "";
  document.getElementById("updatedAt").textContent = data.updatedAt
    ? `updated ${new Date(data.updatedAt * 1000).toLocaleTimeString()}`
    : "not yet polled";
  const srCapableCount = data.graph.nodes.filter(n => n.srCapable).length;
  const nonSrCount = data.graph.nodes.length - srCapableCount;
  document.getElementById("counts").textContent =
    `${data.graph.nodes.length} node(s) (${srCapableCount} SR-capable, ${nonSrCount} non-SR), `
    + `${data.graph.edges.length} link(s), ${data.policies.length} polic(y/ies)`;

  latestPolicies = data.policies;
  latestNodesById = Object.fromEntries(data.graph.nodes.map(n => [n.id, n]));
  updateGraph(data.graph);

  if (selectedNodeId) {
    if (latestNodesById[selectedNodeId]) {
      renderSidebar(selectedNodeId);
    } else {
      clearSelection();
    }
  }

  // Newly-added nodes/edges start with no classes, so a node that now
  // matches the active search wouldn't get highlighted until re-applied.
  if (searchQuery) applySearch(searchQuery);
}

function updateGraph(graph) {
  const currentNodeIds = new Set(cy.nodes().map(n => n.id()));
  const currentEdgeIds = new Set(cy.edges().map(e => e.id()));
  const nextNodeIds = new Set(graph.nodes.map(n => n.id));
  const nextEdgeIds = new Set(graph.edges.map(e => e.id));

  let addedAny = false;

  for (const n of graph.nodes) {
    const displayLabel = nodeDisplayLabel(n);
    if (currentNodeIds.has(n.id)) {
      cy.getElementById(n.id).data({ ...n, displayLabel });
    } else {
      cy.add({ group: "nodes", data: { ...n, displayLabel } });
      addedAny = true;
    }
  }
  cy.nodes().forEach(ele => {
    if (!nextNodeIds.has(ele.id())) ele.remove();
  });

  for (const e of graph.edges) {
    if (currentEdgeIds.has(e.id)) {
      cy.getElementById(e.id).data(e);
    } else {
      cy.add({ group: "edges", data: e });
      addedAny = true;
    }
  }
  cy.edges().forEach(ele => {
    if (!nextEdgeIds.has(ele.id())) ele.remove();
  });

  if (!layoutRan || addedAny) {
    runLayout(!layoutRan);
  }
}

function runLayout(randomize) {
  cy.layout({
    name: "cose",
    animate: false,
    randomize,
    padding: 60,
    // Without this, cose only avoids overlapping the small node circles and
    // ignores label size entirely - labels stack on top of each other once
    // there are more than a handful of nodes.
    nodeDimensionsIncludeLabels: true,
    idealEdgeLength: 120,
    nodeRepulsion: 700000,
    numIter: 2000,
  }).run();
  layoutRan = true;
}

function renderSidebar(nodeId) {
  selectedNodeId = nodeId;
  const node = latestNodesById[nodeId];
  document.getElementById("sidebar-empty").classList.add("hidden");
  document.getElementById("sidebar-content").classList.remove("hidden");
  document.getElementById("nodeTitle").textContent = node.label;
  document.getElementById("nodeMeta").textContent =
    `routerID: ${node.id}${node.sid !== null && node.sid !== undefined ? ` | SID: ${node.sid}` : " | no Node SID"}`
    + (node.isSession ? " | PCEP session" : "");

  // Only policies originating at this node - not ones merely terminating
  // on it or transiting through it.
  const relevant = latestPolicies.filter(p => p.srcRouterId === nodeId);

  const list = document.getElementById("policyList");
  list.innerHTML = "";
  if (!relevant.length) {
    const li = document.createElement("li");
    li.textContent = "No policies sourced from this node.";
    li.style.cursor = "default";
    list.appendChild(li);
    return;
  }

  for (const p of relevant) {
    const li = document.createElement("li");
    li.className = p.policyName === activePolicyName ? "active" : "";
    const unresolvedNote = p.unresolvedSegments && p.unresolvedSegments.length
      ? `<div class="unresolved">${p.unresolvedSegments.length} segment(s) could not be mapped to a node</div>`
      : "";
    const approximateNote = p.approximateHops && p.approximateHops.length
      ? `<div class="unresolved">${p.approximateHops.length} hop(s) shown as a direct line - no all-SR route found between them</div>`
      : "";
    li.innerHTML = `
      <span class="name">${p.policyName}</span>
      <span class="sub">${p.peerAddr} &rarr; color ${p.color ?? "-"} &middot; ${p.state ?? "unknown"} &middot; ${p.type ?? "?"}</span>
      ${unresolvedNote}
      ${approximateNote}
    `;
    li.addEventListener("click", () => highlightPolicy(p));
    list.appendChild(li);
  }
}

function clearSelection() {
  selectedNodeId = null;
  document.getElementById("sidebar-empty").classList.remove("hidden");
  document.getElementById("sidebar-content").classList.add("hidden");
  clearHighlight();
}

function nodeMatchesSearch(node, query) {
  const q = query.trim().toLowerCase();
  if (!q) return false;
  if (node.label.toLowerCase().includes(q)) return true;
  if (node.id.toLowerCase().includes(q)) return true;
  if (node.sid !== null && node.sid !== undefined && String(node.sid).startsWith(q)) return true;
  return false;
}

function applySearch(query) {
  searchQuery = query;
  clearHighlightStyles();
  activePolicyName = null;

  const resultsEl = document.getElementById("searchResults");
  if (!query.trim()) {
    resultsEl.textContent = "";
    return;
  }

  const matches = cy.nodes().filter(n => nodeMatchesSearch(n.data(), query));
  matches.addClass("search-match");
  cy.elements().not(matches).addClass("dimmed");

  resultsEl.textContent = matches.length
    ? `${matches.length} match${matches.length === 1 ? "" : "es"}`
    : "no matches";

  if (matches.length) cy.fit(matches, 80);

  if (selectedNodeId) renderSidebar(selectedNodeId);
}

function highlightPolicy(policy) {
  clearHighlight();
  activePolicyName = policy.policyName;

  const pathNodeIds = new Set(policy.path || []);
  const pathEdgeIds = new Set(policy.pathEdges || []);

  cy.nodes().forEach(n => {
    if (pathNodeIds.has(n.id())) n.addClass("highlighted");
    else n.addClass("dimmed");
  });
  cy.edges().forEach(e => {
    if (pathEdgeIds.has(e.id())) e.addClass("highlighted");
    else e.addClass("dimmed");
  });

  const highlighted = cy.elements(".highlighted");
  if (highlighted.length) cy.fit(highlighted, 60);

  if (selectedNodeId) renderSidebar(selectedNodeId); // refresh "active" styling in the list
}

function clearHighlightStyles() {
  cy.elements().removeClass("highlighted dimmed search-match");
}

function clearHighlight() {
  activePolicyName = null;
  clearHighlightStyles();
  document.getElementById("search").value = "";
  searchQuery = "";
  document.getElementById("searchResults").textContent = "";
}

cy.on("tap", "node", evt => renderSidebar(evt.target.id()));
cy.on("tap", evt => {
  if (evt.target === cy) clearSelection();
});

document.getElementById("clearHighlight").addEventListener("click", clearHighlight);
document.getElementById("relayout").addEventListener("click", () => runLayout(true));

const searchInput = document.getElementById("search");
searchInput.addEventListener("input", () => applySearch(searchInput.value));
searchInput.addEventListener("keydown", evt => {
  if (evt.key === "Escape") clearHighlight();
});

poll();
setInterval(poll, POLL_INTERVAL_MS);
