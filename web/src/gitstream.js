import { S, apiPost, doc_ } from './state.js';
import { updateSidebarToggleState, patchTreeGitStatus, treeEl, setSidebarMode } from './tree.js';
import { drawTabs, loadGutter, closeTab, reloadOpenTabs } from './tabs.js';
import { syncDiffView } from './diff.js';
import { render } from './renderer.js';
import { updateStatus, updateMetricsDisplay } from './status.js';
import { updateGitPanel } from './gitpanel.js';

let eventSource = null;
let reconnectTimer = null;

export function initGitStream() {
  connect();

  // Instant refresh when user focuses the browser window
  window.addEventListener('focus', () => {
    if (document.visibilityState === 'visible') {
      if (!eventSource || eventSource.readyState === EventSource.CLOSED) {
        connect();
      }
      triggerRefresh();
    }
  });

  // Page Visibility API: pause streaming when tab is hidden, resume and refresh when visible
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') {
      disconnect();
    } else {
      connect();
      triggerRefresh();
    }
  });
}

export async function triggerRefresh() {
  if (!S.meta?.git) return;
  try {
    const data = await apiPost('/api/git/refresh');
    await handleGitStatus(data);
  } catch (e) {
    // Quiet fail on network hiccups
  }
}

function connect() {
  if (eventSource && eventSource.readyState !== EventSource.CLOSED) return;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }

  try {
    const streamUrl = new URL('api/stream', document.baseURI || location.href).href;
    eventSource = new EventSource(streamUrl);

    eventSource.addEventListener('git-status', async e => {
      try {
        const data = JSON.parse(e.data);
        await handleGitStatus(data);
      } catch (err) {
        // Drop malformed frame
      }
    });

    eventSource.addEventListener('metrics', e => {
      try {
        const data = JSON.parse(e.data);
        updateMetricsDisplay(data);
      } catch (err) {
        // Drop malformed frame
      }
    });

    eventSource.onerror = () => {
      disconnect();
      if (document.visibilityState === 'visible') {
        reconnectTimer = setTimeout(connect, 3000);
      }
    };
  } catch (err) {
    // Fallback if EventSource fails to construct
  }
}

function disconnect() {
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  if (eventSource) {
    eventSource.close();
    eventSource = null;
  }
}

async function handleGitStatus(data) {
  if (!data) return;

  if (data.gitChanges !== undefined) S.meta.gitChanges = data.gitChanges;
  if (data.gitFiles !== undefined) S.meta.gitFiles = data.gitFiles;

  updateSidebarToggleState();
  if (treeEl?.classList.contains('changed-only') && (!S.meta?.gitChanges || S.meta.gitChanges <= 0)) {
    await setSidebarMode('files');
  }

  const statuses = data.statuses || {};
  const dirtyDirs = data.dirtyDirs || {};
  const staged = data.staged || {};

  // Patch rendered tree items in place without full DOM reload
  await patchTreeGitStatus(statuses, dirtyDirs, staged);
  updateGitPanel(data);

  // Close tabs that were opened in git diff view or currently in diff view if their changes are gone.
  // In PR review mode, tabs should remain open even if clean relative to HEAD.
  if (!S.meta?.pr) {
    for (let i = S.tabs.length - 1; i >= 0; i--) {
      const t = S.tabs[i];
      const code = statuses[t.path];
      const isDiff = !!code && code !== 'U';
      const wasDiff = !!(t.diffMode || t.openedInDiffView);
      if (wasDiff && (t.diffAvailable || t.diffMode) && !isDiff) {
        closeTab(i);
      }
    }
  }

  // Check if any open tabs are affected by modifications. `touched` names files
  // whose content moved even though their status did not (a second edit to a
  // modified or untracked file), so untracked tabs reload too.
  const touched = new Set(data.touched || []);
  const anyTabModified = S.tabs.some(t => {
    const code = statuses[t.path];
    return (code && code !== 'U') || touched.has(t.path);
  });

  if (anyTabModified) {
    // In-place reload of open tabs updates file lines, syntax highlighting, and diff view live
    await reloadOpenTabs();
  } else {
    // Synchronize open tabs' diff badges
    let tabsChanged = false;
    for (const t of S.tabs) {
      const code = statuses[t.path];
      const isDiff = !!code && code !== 'U';
      if (t.diffAvailable !== isDiff) {
        t.diffAvailable = isDiff;
        tabsChanged = true;
      }
    }
    if (tabsChanged) {
      drawTabs();
    }

    // Update active editor gutter and diff view if active document is affected
    const curDoc = doc_();
    if (curDoc) {
      const curCode = statuses[curDoc.path];
      const hasDiff = !!curCode && curCode !== 'U';

      if (curDoc.diffAvailable !== hasDiff || curCode) {
        curDoc.diffAvailable = hasDiff;
        await loadGutter(curDoc);
        render();
        if (curDoc.diffMode) {
          syncDiffView(true);
        }
        updateStatus();
      }
    }
  }
}
