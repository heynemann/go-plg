// A reward deliverer written in TypeScript: it credits the user's points in the wallet API. The platform may call
// it again for the same reward (a call cut short by its deadline may have reached the wallet), so every request
// carries the reward's idempotency key, and the wallet applies a key once: a repeated key answers 200 with the
// first credit, a new one 201. The wallet's address and bearer token are the secrets wallet_api_url and
// wallet_api_token (plugin.yaml declares them), which the platform resolves. Its types are plg.d.ts's (plg types
// writes them).
class Deliverer implements delivery.Deliverer {
  async Deliver(reward: delivery.Reward): Promise<delivery.Receipt> {
    const response = await fetch(`${secrets.get("wallet_api_url")}/credits`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${secrets.get("wallet_api_token")}`,
        "Content-Type": "application/json",
        "Idempotency-Key": reward.idempotencyKey,
      },
      body: JSON.stringify({ user: reward.user, campaign: reward.campaign, points: reward.points, memo: reward.memo }),
    });
    // The wallet's own words, trimmed, go into the error: plg replaces any secret they echo with [secret <name>].
    if (response.status !== 200 && response.status !== 201) {
      const said = (await response.text()).trim();
      throw new Error(`the wallet answered ${response.status} for ${reward.idempotencyKey}: ${said}`);
    }
    const credit: { id?: unknown } = await response.json();
    if (typeof credit.id !== "string" || credit.id === "") {
      throw new Error(`the wallet answered ${response.status} for ${reward.idempotencyKey} without a credit id`);
    }
    return { id: credit.id, duplicate: response.status === 200 };
  }
}

export function Create(): delivery.Deliverer {
  return new Deliverer();
}
