// The browser half of the weather seller's 402 page: it pays the resource with
// the Adena wallet the way a stock x402 client does, shows the live terms, and
// writes the weather it bought into the page's sky.
// The seller inlines this file into the page, so it stays one self-contained
// module: no imports, no build step, no request beyond the seller itself.

const NETWORK_PREFIX = "gno:";

// tm2's denomination grammar, as the client mechanism checks it: the coin sent
// is amount + asset as one string, so an asset outside it signs a different
// coin.
const DENOMINATION = /^[a-z/][a-z0-9_.:/-]{2,273}$/;
const AMOUNT = /^[1-9][0-9]*$/;

// The longest memo a payment may carry, in UTF-8 bytes, as the facilitator
// counts it.
const MAX_MEMO_BYTES = 256;

/** Encodes value as an x402 header carries it: base64 of its JSON, UTF-8. */
export function encodeHeader(value) {
  const bytes = new TextEncoder().encode(JSON.stringify(value));
  return btoa(Array.from(bytes, (b) => String.fromCharCode(b)).join(""));
}

/** Decodes an x402 header back to its JSON value. */
export function decodeHeader(header) {
  const bytes = Uint8Array.from(atob(header), (c) => c.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes));
}

// Asking for JSON is what makes the seller answer with the 402's headers
// rather than with this page again.
async function request(fetchFn, url, headers = {}) {
  return fetchFn(url, { headers: { Accept: "application/json", ...headers } });
}

/** Returns the terms the seller answers an unpaid request with. */
export async function readTerms(fetchFn, url) {
  const response = await request(fetchFn, url);
  const header = response.headers.get("PAYMENT-REQUIRED");
  if (response.status !== 402 || header === null) {
    throw new Error(`the seller answered ${response.status} without payment terms`);
  }
  return decodeHeader(header);
}

// firstText returns the first of values that is a non-empty string, or "".
function firstText(...values) {
  return values.find((v) => typeof v === "string" && v !== "") ?? "";
}

// The gno entry of the offer and the memo its payment carries, checked the way
// the client mechanism checks them before anything is signed. A key of extra
// set to null names nothing, as upstream's SDKs read it.
function gnoOffer(terms) {
  const accepted = (terms.accepts ?? []).find(
    (a) => a.scheme === "exact" && typeof a.network === "string" && a.network.startsWith(NETWORK_PREFIX),
  );
  if (accepted === undefined) {
    throw new Error("this resource offers no gno payment");
  }
  if (!AMOUNT.test(accepted.amount) || !DENOMINATION.test(accepted.asset)) {
    throw new Error(`${accepted.amount} ${accepted.asset} is not a gno coin`);
  }
  if (typeof accepted.payTo !== "string" || accepted.payTo === "") {
    throw new Error("the offer names no payee");
  }
  const method = accepted.extra?.assetTransferMethod;
  if (method !== undefined && method !== null && method !== "bank-send") {
    throw new Error(`the offer pays by ${String(method)}, not by a bank send`);
  }
  const flow = accepted.extra?.paymentFlow;
  if (flow !== undefined && flow !== null && flow !== "authorization") {
    throw new Error(`the offer's payment flow is ${String(flow)}, not authorization`);
  }
  const memo = accepted.extra?.memo;
  if (memo === undefined) {
    return { accepted, memo: "" };
  }
  if (typeof memo !== "string") {
    throw new Error(`the offer's memo is a ${typeof memo}, not text`);
  }
  const bytes = new TextEncoder().encode(memo).length;
  if (bytes > MAX_MEMO_BYTES) {
    throw new Error(`the offer's memo is ${bytes} bytes, over the ${MAX_MEMO_BYTES} a payment can carry`);
  }
  return { accepted, memo };
}

// A payment sent whose answer never came back, which may have settled.
class PaymentUnanswered extends Error {}

// What Adena's refusals mean to the payer, by the type Adena gives them. Each
// stops the payment before anything is signed or sent.
const ADENA_REFUSALS = new Map([
  ["CONNECTION_REJECTED", "Adena did not connect to this page: nothing was signed or sent."],
  ["SWITCH_NETWORK_REJECTED", "Adena stayed on its network: nothing was signed or sent."],
  ["WALLET_LOCKED", "Adena is locked: unlock it, then pay again."],
  ["NO_ACCOUNT", "Adena has no account yet: create one, then pay again."],
  ["TRANSACTION_REJECTED", "Declined in Adena: nothing was signed or sent."],
  ["SIGN_REJECTED", "Declined in Adena: nothing was signed or sent."],
]);

// Adena answers ALREADY_CONNECTED and REDUNDANT_CHANGE_REQUEST as failures
// although they mean the wallet is already where the page wants it.
function expectSuccess(response, alsoFine = []) {
  if (response?.status === "success" || alsoFine.includes(response?.type)) {
    return response.data;
  }
  const plain = ADENA_REFUSALS.get(response?.type);
  if (plain !== undefined) {
    throw new Error(plain);
  }
  const said = firstText(response?.message, response?.type);
  throw new Error(`Adena: ${said === "" ? "no answer" : said}`);
}

/**
 * Pays url with Adena: reads the terms, has Adena sign a bank send of the
 * amount to the payee on the offer's chain without broadcasting it, and
 * repeats the request with that payment.
 *
 * Throws before anything is sent for a wallet refusal, a missing network or an
 * offer the page cannot pay, and after sending when no answer came back.
 * Otherwise returns what the paid request answered.
 *
 * report hears each stage with the step of the exchange it belongs to: "402"
 * while the terms are read, "sign" while Adena connects and signs, "retry"
 * while the paid request goes out.
 */
export async function payWithAdena({ adena, fetch: fetchFn, url, report = () => {} }) {
  report("Reading the terms…", "402");
  const terms = await readTerms(fetchFn, url);
  const { accepted, memo } = gnoOffer(terms);
  const chainId = accepted.network.slice(NETWORK_PREFIX.length);

  report("Connecting to Adena…", "sign");
  expectSuccess(await adena.AddEstablish("x402 weather demo"), ["ALREADY_CONNECTED"]);
  const network = expectSuccess(await adena.GetNetwork());
  if (network.chainId !== chainId) {
    report(`Switching Adena to ${chainId}…`, "sign");
    const switched = await adena.SwitchNetwork(chainId);
    if (switched?.type === "UNADDED_NETWORK") {
      throw new Error(`Adena has no network ${chainId}: add it as a custom network in Adena, then pay again.`);
    }
    expectSuccess(switched, ["REDUNDANT_CHANGE_REQUEST"]);
  }
  const account = expectSuccess(await adena.GetAccount());

  report("Confirm the payment in Adena…", "sign");
  const signed = expectSuccess(
    await adena.SignTx({
      messages: [
        {
          type: "/bank.MsgSend",
          value: {
            from_address: account.address,
            to_address: accepted.payTo,
            amount: `${accepted.amount}${accepted.asset}`,
          },
        },
      ],
      memo,
    }),
  );

  report("Paying…", "retry");
  const payment = {
    x402Version: terms.x402Version,
    resource: terms.resource,
    accepted,
    payload: { transaction: signed.encodedTransaction },
  };
  let response;
  let body;
  try {
    response = await request(fetchFn, url, { "PAYMENT-SIGNATURE": encodeHeader(payment) });
    body = await response.text();
  } catch (err) {
    const cause = err instanceof Error ? err.message : String(err);
    throw new PaymentUnanswered(
      `The payment was sent, but no answer came back (${cause}). Check the account, and if the payment never left it, reload this page to pay again.`,
    );
  }

  // A settle failure carries its reason in the receipt, a verify refusal in a
  // fresh challenge.
  const receiptHeader = response.headers.get("PAYMENT-RESPONSE");
  const challengeHeader = response.headers.get("PAYMENT-REQUIRED");
  const receipt = receiptHeader === null ? {} : decodeHeader(receiptHeader);
  const challenge = challengeHeader === null ? {} : decodeHeader(challengeHeader);
  const paid = response.ok && receipt.success === true;
  return {
    paid,
    settlementFailed: receiptHeader !== null && !paid,
    status: response.status,
    body,
    transaction: receipt.transaction ?? "",
    network: accepted.network,
    reason: firstText(receipt.errorReason, challenge.error),
  };
}

// What a payer can do about a refusal, for the reasons whose remedy the
// facilitator's vocabulary names. Any other reason shows as the seller sent it.
const REMEDIES = new Map([
  [
    "invalid_exact_gno_transaction_simulation_failed",
    "the chain would not run this payment as signed. Check that the account holds the amount and the fee, then pay again",
  ],
  [
    "invalid_exact_gno_payload_signature_invalid",
    "the signature does not match the paying account on this chain. Pay again from the account Adena shows",
  ],
  ["invalid_exact_gno_payload_sequence_mismatch", "this payment was already spent. Pay again to sign a fresh one"],
]);

// The reasons after which a payment may still settle, or already has.
const UNSETTLED = ["settlement_pending", "duplicate_settlement"];

// The chain refused the payment: in the mempool, which charges nothing, or in a
// block, which charges the fee. Only the hash would tell which, and the weather
// seller drops it from a failed settlement.
const CHAIN_REFUSED = "invalid_exact_gno_transaction_failed";

// mayStillSettle says whether a payment that came back unpaid may still settle,
// or already has, so the page offers no second payment. Besides a pending or
// duplicate settlement, that is a settlement that failed with neither a hash
// nor a refusal's reason: its facilitator could not tell whether the chain took
// the payment.
function mayStillSettle(result) {
  if (UNSETTLED.includes(result.reason)) {
    return true;
  }
  return (
    result.settlementFailed === true &&
    result.transaction === "" &&
    !REMEDIES.has(result.reason) &&
    result.reason !== CHAIN_REFUSED
  );
}

/**
 * Says what a payment came to, what the payer can do first and the reason's
 * code last. A payment the chain admitted but has not yet executed comes back
 * unpaid with its transaction, and may still go through.
 */
export function statusLine(result) {
  if (result.paid && result.from === "code") {
    return `Paid from code by ${shortAddress(result.payer)} on ${result.network}. Transaction ${result.transaction}.`;
  }
  if (result.paid) {
    return `Paid on ${result.network}. Transaction ${result.transaction}.`;
  }
  if (result.reason === "settlement_pending") {
    return `Sent, not settled yet: the chain has the payment but has not run it, so the weather was held back. Transaction ${result.transaction} may still go through. Check it, and if it does not, reload this page to pay again (settlement_pending).`;
  }
  if (result.reason === "duplicate_settlement") {
    return "Already settling: this payment is being settled or was settled. Check the account, and if the payment never left it, reload this page to pay again (duplicate_settlement).";
  }
  const remedy = REMEDIES.get(result.reason);
  if (remedy !== undefined) {
    return `Not paid: ${remedy} (${result.reason}).`;
  }
  if (result.transaction !== "") {
    return `Not paid: the chain refused transaction ${result.transaction} (${result.reason}).`;
  }
  if (result.reason === CHAIN_REFUSED) {
    return `Not paid: the chain refused this payment, and may have charged its network fee (${result.reason}).`;
  }
  if (mayStillSettle(result)) {
    return `Sent, not confirmed: the seller could not tell whether this payment went through, so the weather was held back. Check the account, and if the payment never left it, reload this page to pay again (${result.reason}).`;
  }
  // A refusal in a fresh challenge came before anything was broadcast, so
  // nothing left the account.
  if (result.reason !== "") {
    return `Not paid, and nothing was spent: the seller refused this payment (${result.reason}).`;
  }
  return `Not paid (${result.status}).`;
}

function shortAddress(address) {
  return address.length > 12 ? `${address.slice(0, 6)}…${address.slice(-4)}` : address;
}

// showStatus writes line into the status line, its transaction linked to the
// chain's explorer page for it, which the link names by its host. data-tx is
// that page less the hash, and empty on a chain without an explorer. A screen
// reader hears the transaction by its first eight characters, not all 64.
function showStatus(document, status, line, transaction) {
  const at = transaction === "" ? -1 : line.indexOf(transaction);
  if (at === -1) {
    status.textContent = line;
    return;
  }
  const explorer = status.dataset.tx ?? "";
  const hash = document.createElement(explorer === "" ? "span" : "a");
  const full = document.createElement("span");
  full.setAttribute("aria-hidden", "true");
  full.textContent = transaction;
  const short = document.createElement("span");
  short.className = "visually-hidden";
  short.textContent = `${transaction.slice(0, 8)}…`;
  hash.append(full, short);
  if (explorer !== "") {
    hash.setAttribute("href", explorer + encodeURIComponent(transaction));
    hash.setAttribute("target", "_blank");
    hash.setAttribute("rel", "noopener");
    hash.append(` on ${new URL(explorer).host}`);
  }
  status.replaceChildren(line.slice(0, at), hash, line.slice(at + transaction.length));
}

// The forecasts the page has a sky and an icon for; the icon of each is the
// symbol #i-<forecast>.
const FORECASTS = ["sunny", "cloudy", "rain", "snow"];

// readingOf returns the reading the sky can draw from a paid body, or null for
// any other body, which the page then shows only as served.
function readingOf(body) {
  let weather;
  try {
    weather = JSON.parse(body);
  } catch {
    return null;
  }
  if (typeof weather?.celsius !== "number" || !FORECASTS.includes(weather.forecast)) {
    return null;
  }
  return { celsius: weather.celsius, forecast: weather.forecast };
}

// How long the page waits for Adena to inject window.adena: it does so from a
// script it loads itself, which can land after the page's load event.
const ADENA_WAIT_MS = 5000;
const ADENA_POLL_MS = 250;

// The tab leads with the payment's state, as the badge does.
const TITLE_UNPAID = "Payment required";
const TITLE_SETTLED = "Payment settled";
const TITLE_SENT = "Payment sent";

// The tab's visit, which the commands that pay this page carry so the seller
// can tell the page of a payment made from a terminal. sessionStorage keeps it
// across a reload; where the browser refuses storage it lasts as long as the
// page.
const VISIT_KEY = "x402-weather-visit";
// Set once a command carrying the visit is copied, so a reload watches on.
const WATCHING_KEY = "x402-weather-watching";
const VISIT_TOKEN = /^[0-9a-f]{32}$/;

// How often the page asks the seller about its visit while in view, and for
// how long after a copy or a return to the tab: funding a fresh key takes
// minutes.
const VISIT_POLL_MS = 3000;
const VISIT_WATCH_MS = 10 * 60_000;

function stored(window, key) {
  try {
    return window.sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

function store(window, key, value) {
  try {
    window.sessionStorage.setItem(key, value);
  } catch {
    // Refused: the page works on, and forgets on reload.
  }
}

// visitToken returns this tab's visit, made from 128 random bits the first
// time.
function visitToken(window) {
  const kept = stored(window, VISIT_KEY);
  if (kept !== null && VISIT_TOKEN.test(kept)) {
    return kept;
  }
  const bytes = window.crypto.getRandomValues(new Uint8Array(16));
  const token = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  store(window, VISIT_KEY, token);
  return token;
}

// lookUpVisit returns the payment the seller kept under the visit url names,
// or null while there is none or the seller cannot say.
async function lookUpVisit(window, url) {
  try {
    const response = await window.fetch(url, { cache: "no-store", headers: { Accept: "application/json" } });
    if (response.status !== 200) {
      return null;
    }
    const found = await response.json();
    return ["transaction", "network", "payer", "body"].every((key) => typeof found?.[key] === "string") ? found : null;
  } catch {
    return null;
  }
}

/**
 * Writes url as one shell word: bare when a shell reads none of its
 * characters, single-quoted otherwise.
 */
export function shellWord(url) {
  return /^[\w@%+=:,./-]+$/.test(url) ? url : `'${url.replaceAll("'", `'\\''`)}'`;
}

/** Wires the page: this page's URL, copy buttons, the live terms, and Adena. */
export function start(document, window) {
  // A fragment comes from a link inside this page, not from the seller, and a
  // visit in the query names another tab.
  const { origin, pathname, search } = window.location;
  const own = new URL(origin + pathname + search);
  own.searchParams.delete("visit");
  const here = own.href;
  for (const e of document.querySelectorAll("[data-url]")) {
    e.textContent = shellWord(here);
  }
  const visit = visitToken(window);
  const paying = new URL(here);
  paying.searchParams.set("visit", visit);
  for (const e of document.querySelectorAll("[data-pay-url]")) {
    e.textContent = shellWord(paying.href);
  }

  if (window.navigator.clipboard !== undefined) {
    for (const pre of document.querySelectorAll("pre[data-copy]")) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "copy";
      button.textContent = "Copy";
      button.addEventListener("click", async () => {
        const text = pre.querySelector("code").innerText;
        try {
          await window.navigator.clipboard.writeText(text);
          button.textContent = "Copied";
        } catch {
          button.textContent = "Copy failed";
        }
        window.setTimeout(() => (button.textContent = "Copy"), 1500);
        if (text.includes(visit)) {
          watch();
        }
      });
      pre.append(button);
    }
  }
  document.addEventListener("copy", () => {
    if (String(window.getSelection()).includes(visit)) {
      watch();
    }
  });

  // A phone pays on neither path, Adena being a desktop extension and the buyer
  // a terminal command, so the page hands its own link to the share sheet, or to
  // the clipboard where there is none, for a computer to open.
  const send = document.getElementById("send-link");
  send.addEventListener("click", async () => {
    if (window.navigator.share !== undefined) {
      try {
        await window.navigator.share({ title: document.title, url: here });
        return;
      } catch (err) {
        // A dismissed sheet is the visitor's choice; any other failure falls back
        // to the clipboard.
        if (err?.name === "AbortError") {
          return;
        }
      }
    }
    const label = send.textContent;
    try {
      await window.navigator.clipboard.writeText(here);
      send.textContent = "Link copied";
    } catch {
      send.textContent = "Copy failed";
    }
    window.setTimeout(() => (send.textContent = label), 1500);
  });

  const terms = document.getElementById("terms");
  let termsRead = false;
  let termsReading = false;
  terms.addEventListener("toggle", async () => {
    if (termsRead || termsReading || !terms.open) {
      return;
    }
    const out = document.getElementById("terms-json");
    termsReading = true;
    try {
      out.textContent = JSON.stringify(await readTerms(window.fetch.bind(window), here), null, 2);
      termsRead = true;
    } catch (err) {
      out.textContent = String(err);
    } finally {
      termsReading = false;
    }
  });

  // Presence alone is not the wallet: an element whose id is "adena" would be
  // window.adena too.
  const offerAdena = () => {
    if (typeof window.adena?.SignTx !== "function") {
      return false;
    }
    document.getElementById("no-adena").hidden = true;
    document.getElementById("pay-adena").hidden = false;
    return true;
  };
  if (!offerAdena()) {
    let waited = 0;
    const poll = window.setInterval(() => {
      waited += ADENA_POLL_MS;
      if (offerAdena() || waited >= ADENA_WAIT_MS) {
        window.clearInterval(poll);
      }
    }, ADENA_POLL_MS);
  }

  const button = document.getElementById("pay");
  const status = document.getElementById("pay-status");
  const result = document.getElementById("pay-result");
  // The sky's data-step lights the hero's track; without one it rests on the
  // unpaid request.
  const sky = document.getElementById("sky");
  // A payment that settled, or may still, withdraws the button, so a second
  // click cannot sign a second payment for the same reading.
  const settle = (outcome) => {
    status.dataset.outcome = outcome;
    document.title = document.title.replace(TITLE_UNPAID, outcome === "paid" ? TITLE_SETTLED : TITLE_SENT);
    // Hiding the button that holds focus would drop it to the top of the page.
    if (document.activeElement === button) {
      status.focus();
    }
    button.hidden = true;
  };
  // After a click, focus moves to the status line, since the button that held
  // it is gone.
  const withdraw = (outcome) => {
    settle(outcome);
    status.focus();
  };
  // serve draws a paid response: the body, and the reading it carries.
  const serve = (body) => {
    sky.dataset.step = "200";
    result.querySelector("code").textContent = body;
    result.hidden = false;
    const reading = readingOf(body);
    if (reading !== null) {
      document.getElementById("paid-temp").textContent = String(reading.celsius);
      document.getElementById("paid-forecast").textContent = reading.forecast;
      document.getElementById("paid-icon").setAttribute("href", `#i-${reading.forecast}`);
      document.getElementById("paid-reading").textContent = `${reading.celsius} °C, ${reading.forecast}`;
      sky.dataset.forecast = reading.forecast;
    }
  };
  const refuse = () => {
    status.dataset.outcome = "refused";
    delete sky.dataset.step;
  };
  button.addEventListener("click", async () => {
    button.disabled = true;
    result.hidden = true;
    delete status.dataset.outcome;
    try {
      const paid = await payWithAdena({
        adena: window.adena,
        fetch: window.fetch.bind(window),
        url: here,
        report: (message, step) => {
          status.textContent = message;
          sky.dataset.step = step;
        },
      });
      showStatus(document, status, statusLine(paid), paid.transaction);
      if (paid.paid) {
        serve(paid.body);
        withdraw("paid");
      } else if (mayStillSettle(paid)) {
        withdraw("pending");
      } else {
        refuse();
      }
    } catch (err) {
      status.textContent = err instanceof Error ? err.message : String(err);
      if (err instanceof PaymentUnanswered) {
        withdraw("pending");
      } else {
        refuse();
      }
    } finally {
      button.disabled = false;
      // Disabling the button dropped its focus; after a refusal it takes it
      // back, now that it can.
      if (status.dataset.outcome === "refused") {
        button.focus();
      }
    }
  });

  // A payment from code shows once the seller has it under this tab's visit.
  // The page asks while in view and while Adena is not paying, and stops when
  // one shows, when Adena paid, or VISIT_WATCH_MS after the last copy or
  // return to the tab.
  const lookup = `${origin}${pathname}/visits/${visit}`;
  let watching = stored(window, WATCHING_KEY) === "1";
  let watchUntil = 0;
  let next;
  let asking = false;
  const ask = async () => {
    window.clearTimeout(next);
    next = undefined;
    if (asking || Date.now() >= watchUntil || ["paid", "pending"].includes(status.dataset.outcome)) {
      return;
    }
    if (document.visibilityState !== "hidden" && !button.disabled) {
      asking = true;
      const found = await lookUpVisit(window, lookup);
      asking = false;
      // Adena may have started or ended a payment while the seller answered.
      if (found !== null && !button.disabled && !["paid", "pending"].includes(status.dataset.outcome)) {
        showStatus(document, status, statusLine({ paid: true, from: "code", ...found }), found.transaction);
        serve(found.body);
        settle("paid");
        return;
      }
    }
    next = window.setTimeout(ask, VISIT_POLL_MS);
  };
  const watch = () => {
    watching = true;
    store(window, WATCHING_KEY, "1");
    watchUntil = Date.now() + VISIT_WATCH_MS;
    void ask();
  };
  document.addEventListener("visibilitychange", () => {
    if (watching && document.visibilityState === "visible") {
      watch();
    }
  });
  if (watching) {
    watch();
  }
}
