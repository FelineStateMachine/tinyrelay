// Enable only base Git hosting on an isolated, owned benchmark tenant.
import { finalizeEvent } from "nostr-tools/pure";
import { getToken } from "nostr-tools/nip98";

const url = process.argv[2] || "http://127.0.0.1:17447";
const privateRepos = process.argv.includes("--private");
const secret = Uint8Array.from(Buffer.from("fc1d06a0fd5e622dcf448d0b3c2fccc891a5ecf74546a7675460d92441fecfdd", "hex"));
async function rpc(method, params = []) {
  const payload = {method, params};
  const token = await getToken(url, "POST", event => finalizeEvent(event, secret), true, payload);
  const response = await fetch(url, {method: "POST", headers: {
    "Content-Type": "application/nostr+json+rpc", Authorization: token,
  }, body: JSON.stringify(payload)});
  const result = await response.json();
  if (!response.ok || result.error) throw Error(JSON.stringify(result));
  return result.result;
}
const policy = await rpc("getpolicy");
policy.features = {...policy.features, grasp: true, grasp02: false, grasp03: false, grasp05: false, grasp06: false, grasp08: privateRepos};
if (privateRepos) policy.reads = "members";
console.log(JSON.stringify(await rpc("setpolicy", [policy])));
