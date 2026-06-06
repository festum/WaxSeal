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

// Go invokes these functions by name through wx_call. Keep them non-enumerable
// because browsers do not expose them.
const defHidden = (name, value) =>
  Object.defineProperty(G, name, { value, configurable: true, writable: true, enumerable: false });

// The snapshot signal and minter must survive across calls without becoming
// browser globals.
let webPoSignalOutput;
let minter;

/**
 * Apply a coherent BrowserProfile (optional), load the fetched interpreter,
 * create the BotGuard client, and retain the snapshot signal for newMinter().
 *
 * @param {string} interpreterJavascript - descrambled interpreter JS (or fake VM)
 * @param {string} program - challenge program (arr[4])
 * @param {string} globalName - VM global name (arr[5])
 * @param {object=} profile - optional BrowserProfile override
 * @returns {Promise<string>} botguardResponse
 */
defHidden('runBotguard', async (interpreterJavascript, program, globalName, profile) => {
  if (profile) G.__wxApplyProfile(profile);

  // Define the VM global (real Google interpreter, or a test fake VM).
  new Function(interpreterJavascript)();

  const botguard = await BG.BotGuardClient.create({
    program,
    globalName,
    globalObj: G
  });

  webPoSignalOutput = [];
  const botguardResponse = await botguard.snapshot({ webPoSignalOutput });
  return botguardResponse;
});

/**
 * Create the WebPoMinter from the GenerateIT integrity token and the live
 * webPoSignalOutput captured during snapshot. Must run on the same runtime as
 * runBotguard because webPoSignalOutput[0] is a live JS closure.
 *
 * @param {string} integrityToken - base64 integrity token from GenerateIT
 * @returns {Promise<boolean>}
 */
defHidden('newMinter', async (integrityToken) => {
  minter = await BG.WebPoMinter.create({ integrityToken }, webPoSignalOutput);
  return true;
});

/**
 * Mint a websafe-base64 PO token bound to `identifier` (visitor_data or
 * video_id). One minter mints many identifiers until expiry.
 *
 * @param {string} identifier
 * @returns {Promise<string>} websafe base64 token
 */
defHidden('mint', async (identifier) => {
  return await minter.mintAsWebsafeString(identifier);
});
