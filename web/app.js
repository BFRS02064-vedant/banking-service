"use strict";

// ---------------------------------------------------------------------------
// Banner helpers
// ---------------------------------------------------------------------------

function showBanner(msg, isError) {
  const banner = document.getElementById("banner");
  const text = document.getElementById("banner-msg");
  text.textContent = msg; // textContent — no innerHTML
  banner.className = "banner " + (isError ? "banner-error" : "banner-success");
  banner.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function hideBanner() {
  document.getElementById("banner").className = "banner hidden";
}

// ---------------------------------------------------------------------------
// API helpers
// ---------------------------------------------------------------------------

async function apiFetch(method, path, body, headers) {
  const opts = {
    method,
    headers: Object.assign({ "Content-Type": "application/json" }, headers || {}),
  };
  if (body !== undefined) {
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = await res.json();
  if (!res.ok) {
    // data.message or data.error as human text — never innerHTML
    const msg = (data && data.message) ? data.message : (data && data.error) ? data.error : "unknown error";
    throw new Error(msg);
  }
  return data;
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

async function fetchAccounts() {
  try {
    const accounts = await apiFetch("GET", "/api/accounts");
    renderAccounts(accounts);
    populateDropdowns(accounts);
  } catch (err) {
    showBanner("Failed to load accounts: " + err.message, true);
  }
}

function renderAccounts(accounts) {
  const tbody = document.getElementById("accounts-body");
  tbody.textContent = ""; // clear
  for (const acc of accounts) {
    const tr = tbody.insertRow();
    const tdId = tr.insertCell();
    const tdName = tr.insertCell();
    const tdBal = tr.insertCell();
    tdId.textContent = acc.ID;
    tdName.textContent = acc.Name;
    tdBal.textContent = acc.Balance; // string from API — never parseFloat
  }
}

function populateDropdowns(accounts) {
  const dropdowns = [
    document.getElementById("transfer-src"),
    document.getElementById("transfer-dst"),
    document.getElementById("deposit-dst"),
    document.getElementById("history-account"),
  ];
  for (const sel of dropdowns) {
    const current = sel.value;
    sel.textContent = ""; // clear options safely
    for (const acc of accounts) {
      const opt = document.createElement("option");
      opt.value = acc.ID;
      opt.textContent = acc.Name; // textContent — XSS safe
      sel.appendChild(opt);
    }
    // Restore previous selection if still present.
    if (current) sel.value = current;
  }
}

async function handleCreateAccount(event) {
  event.preventDefault();
  const name = document.getElementById("account-name").value.trim();
  if (!name) return;
  try {
    await apiFetch("POST", "/api/accounts", { name });
    showBanner("Account created: " + name, false);
    document.getElementById("account-name").value = "";
    await fetchAccounts();
  } catch (err) {
    showBanner("Create account failed: " + err.message, true);
  }
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

async function handleTransfer(event) {
  event.preventDefault();
  const src = document.getElementById("transfer-src").value;
  const dst = document.getElementById("transfer-dst").value;
  const amount = document.getElementById("transfer-amount").value.trim();
  try {
    await apiFetch(
      "POST", "/api/transfers",
      { src_account_id: src, dst_account_id: dst, amount },
      { "Idempotency-Key": crypto.randomUUID() }
    );
    showBanner("Transfer successful", false);
    document.getElementById("transfer-amount").value = "";
    await fetchAccounts();
    await refreshOpenHistory();
  } catch (err) {
    showBanner("Transfer failed: " + err.message, true);
  }
}

// ---------------------------------------------------------------------------
// Deposits
// ---------------------------------------------------------------------------

async function handleDeposit(event) {
  event.preventDefault();
  const dst = document.getElementById("deposit-dst").value;
  const amount = document.getElementById("deposit-amount").value.trim();
  try {
    await apiFetch(
      "POST", "/api/deposits",
      { dst_account_id: dst, amount },
      { "Idempotency-Key": crypto.randomUUID() }
    );
    showBanner("Deposit successful", false);
    document.getElementById("deposit-amount").value = "";
    await fetchAccounts();
    await refreshOpenHistory();
  } catch (err) {
    showBanner("Deposit failed: " + err.message, true);
  }
}

// ---------------------------------------------------------------------------
// Reversals
// ---------------------------------------------------------------------------

async function handleReverseById(event) {
  event.preventDefault();
  const txID = document.getElementById("reverse-txid").value.trim();
  await doReverse(txID);
  document.getElementById("reverse-txid").value = "";
}

async function handleReverseButton(txID) {
  await doReverse(txID);
}

async function doReverse(txID) {
  try {
    await apiFetch(
      "POST", "/api/transfers/" + txID + "/reverse",
      null,
      { "Idempotency-Key": crypto.randomUUID() }
    );
    showBanner("Reversal successful", false);
    await fetchAccounts();
    await refreshOpenHistory();
  } catch (err) {
    showBanner("Reversal failed: " + err.message, true);
  }
}

// ---------------------------------------------------------------------------
// Account History panel (per-account)
// ---------------------------------------------------------------------------

// Track which account is currently shown so refreshOpenHistory can reload it.
let currentHistoryAccountID = null;

async function loadHistory() {
  const sel = document.getElementById("history-account");
  const accountID = sel.value;
  if (!accountID) return;
  currentHistoryAccountID = accountID;
  await Promise.all([
    fetchTransactions(accountID),
    fetchAudit(accountID),
  ]);
}

async function fetchTransactions(accountID) {
  try {
    const txns = await apiFetch("GET", "/api/accounts/" + accountID + "/transactions");
    renderTransactions(txns, false, "txns-body");
  } catch (err) {
    showBanner("Failed to load transactions: " + err.message, true);
  }
}

// renderTransactions renders transaction rows into the given tbody element ID.
// When append is true the table is not cleared first (used by Load more).
function renderTransactions(txns, append, targetTbodyID) {
  const tbody = document.getElementById(targetTbodyID);
  if (!append) {
    tbody.textContent = "";
  }
  for (const tx of txns) {
    const tr = tbody.insertRow();
    const tdId = tr.insertCell();
    const tdKind = tr.insertCell();
    const tdDate = tr.insertCell();
    const tdAction = tr.insertCell();

    tdId.textContent = tx.ID;
    tdKind.textContent = tx.Kind;
    tdDate.textContent = tx.CreatedAt ? new Date(tx.CreatedAt).toLocaleString() : "";

    // Show each entry as "DEBIT amount (AccountName)" on its own line.
    if (Array.isArray(tx.Entries)) {
      for (const entry of tx.Entries) {
        const div = document.createElement("div");
        // Build text: "DEBIT 100.0000 (Alice)" — textContent only, never innerHTML.
        div.textContent = entry.Direction + " " + entry.Amount + " (" + entry.AccountName + ")";
        tdKind.appendChild(div);
      }
    }

    // Reverse button only for TRANSFER rows (REVERSAL and DEPOSIT are not reversible).
    if (tx.Kind === "TRANSFER") {
      const btn = document.createElement("button");
      btn.textContent = "Reverse";
      btn.className = "btn-reverse";
      const txID = tx.ID; // capture
      btn.addEventListener("click", function () { handleReverseButton(txID); });
      tdAction.appendChild(btn);
    }
  }
}

async function fetchAudit(accountID) {
  try {
    const entries = await apiFetch("GET", "/api/accounts/" + accountID + "/audit");
    renderAudit(entries);
  } catch (err) {
    showBanner("Failed to load audit: " + err.message, true);
  }
}

function renderAudit(entries) {
  const tbody = document.getElementById("audit-body");
  tbody.textContent = "";
  for (const e of entries) {
    const tr = tbody.insertRow();
    tr.insertCell().textContent = e.Operation;
    const outcomeCell = tr.insertCell();
    outcomeCell.textContent = e.Outcome;
    outcomeCell.className = e.Outcome === "SUCCESS" ? "outcome-success" : "outcome-failure";
    tr.insertCell().textContent = e.Amount || "";  // already a string from API
    tr.insertCell().textContent = e.ErrorReason || "";
    tr.insertCell().textContent = e.CreatedAt ? new Date(e.CreatedAt).toLocaleString() : "";
  }
}

// ---------------------------------------------------------------------------
// Ledger Activity panel (global paginated)
// ---------------------------------------------------------------------------

let allLedgerOffset = 0;
const allLedgerLimit = 50;

async function fetchLedgerActivity(append) {
  const loadMoreContainer = document.getElementById("all-load-more-container");
  try {
    const txns = await apiFetch(
      "GET",
      "/api/transactions?limit=" + allLedgerLimit + "&offset=" + allLedgerOffset
    );
    renderTransactions(txns, append, "all-txns-body");
    if (txns.length >= allLedgerLimit) {
      loadMoreContainer.classList.remove("hidden");
    } else {
      loadMoreContainer.classList.add("hidden");
    }
  } catch (err) {
    showBanner("Failed to load ledger activity: " + err.message, true);
  }
}

async function loadMoreLedger() {
  allLedgerOffset += allLedgerLimit;
  await fetchLedgerActivity(true);
}

// ---------------------------------------------------------------------------
// refreshOpenHistory — called after any successful mutation
// Reloads the per-account panel (if open) AND resets Ledger Activity to page 0.
// ---------------------------------------------------------------------------

async function refreshOpenHistory() {
  // Reload per-account history if one is open.
  if (currentHistoryAccountID) {
    await Promise.all([
      fetchTransactions(currentHistoryAccountID),
      fetchAudit(currentHistoryAccountID),
    ]);
  }
  // Reset Ledger Activity to first page.
  allLedgerOffset = 0;
  document.getElementById("all-txns-body").textContent = "";
  await fetchLedgerActivity(false);
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

fetchAccounts();
fetchLedgerActivity(false);
