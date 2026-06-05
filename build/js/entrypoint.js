/*
 * WaxSeal BotGuard entrypoint (ported from rustypipe bg_entrypoint.js, MIT,
 * with credit) without JSDOM or fetch. The browser identity is rendered by
 * shim.js from the active BrowserProfile; WaxSeal does all HTTP in Go, so the
 * VM only runs the snapshot + minter. Exposes runBotguard / newMinter / mint
 * on globalThis, driven by the Go wazero backend via wx_call.
 */
import './shim.js';
import { BG } from 'bgutils-js';

const G = globalThis;

/**
 * Apply a coherent BrowserProfile (optional), load the fetched interpreter,
 * create the BotGuard client and take a snapshot. Leaves webPoSignalOutput
 * (whose [0] is the live getMinter closure) on globalThis for newMinter().
 *
 * @param {string} interpreterJavascript - descrambled interpreter JS (or fake VM)
 * @param {string} program - challenge program (arr[4])
 * @param {string} globalName - VM global name (arr[5])
 * @param {object=} profile - optional BrowserProfile override
 * @returns {Promise<string>} botguardResponse
 */
G.runBotguard = async (interpreterJavascript, program, globalName, profile) => {
  if (profile) G.__wxApplyProfile(profile);

  // Define the VM global (real Google interpreter, or a test fake VM).
  new Function(interpreterJavascript)();

  const botguard = await BG.BotGuardClient.create({
    program,
    globalName,
    globalObj: G
  });

  G.webPoSignalOutput = [];
  const botguardResponse = await botguard.snapshot({
    webPoSignalOutput: G.webPoSignalOutput
  });
  return botguardResponse;
};

/**
 * Create the WebPoMinter from the GenerateIT integrity token and the live
 * webPoSignalOutput captured during snapshot. Must run on the same runtime as
 * runBotguard because webPoSignalOutput[0] is a live JS closure.
 *
 * @param {string} integrityToken - base64 integrity token from GenerateIT
 * @returns {Promise<boolean>}
 */
G.newMinter = async (integrityToken) => {
  G.minter = await BG.WebPoMinter.create({ integrityToken }, G.webPoSignalOutput);
  return true;
};

/**
 * Mint a websafe-base64 PO token bound to `identifier` (visitor_data or
 * video_id). One minter mints many identifiers until expiry.
 *
 * @param {string} identifier
 * @returns {Promise<string>} websafe base64 token
 */
G.mint = async (identifier) => {
  return await G.minter.mintAsWebsafeString(identifier);
};

// Marker the Go side can assert the bundle loaded.
G.__wxBundleReady = true;
