// A pricing rule written in TypeScript: the SUMMER coupon takes 8% off, and the promotions team's coupon API says
// what any other coupon takes off. Its types (pricing.Rule, pricing.Order, pricing.Limits, plg.Issue) are
// plg.d.ts's, which plg types writes from the contract; fetch and secrets are plg's globals too.
const coupon = "SUMMER";
// percent is what SUMMER takes off, and the most the rule grants for any coupon.
const percent = 8;

class Rule implements pricing.Rule {
  async Discount(order: pricing.Order): Promise<pricing.Discount> {
    if (order.coupon === "") {
      return { cents: 0, reason: "" };
    }
    const off = order.coupon === coupon ? percent : await couponPercent(order.coupon);
    if (off === 0) {
      return { cents: 0, reason: "" };
    }
    const subtotal = (order.items ?? []).reduce((sum, item) => sum + item.quantity * item.unitCents, 0);
    return { cents: Math.floor((subtotal * off) / 100), reason: `coupon ${order.coupon}: ${off}% off` };
  }

  // Validate rejects the rule when the service lets rules grant less than its percentage.
  Validate(limits: pricing.Limits): plg.Issue[] {
    if (percent <= limits.maxPercent) {
      return [];
    }
    return [{
      severity: "error",
      code: "seasonal.percent",
      message: `${percent}% off, over the ${limits.maxPercent}% limit`,
      field: "percent",
    }];
  }
}

// couponPercent asks the coupon API what code takes off, in percent: GET <coupon_api_url>/coupons/<code> with the
// bearer token coupon_api_token answers {"percent": n}, or 404 for a code it does not know (0: nothing off). Both
// are secrets plugin.yaml declares, which the service resolves (in tests, the cases fake them), so the API's
// address and token change with the environment, never with the rule.
async function couponPercent(code: string): Promise<number> {
  const response = await fetch(`${secrets.get("coupon_api_url")}/coupons/${encodeURIComponent(code)}`, {
    headers: { Authorization: `Bearer ${secrets.get("coupon_api_token")}` },
  });
  if (response.status === 404) {
    return 0;
  }
  // The API's own words, trimmed, go into the error: plg replaces any secret they echo with [secret <name>].
  if (!response.ok) {
    const said = (await response.text()).trim();
    throw new Error(`the coupon API answered ${response.status} for ${code}: ${said}`);
  }
  const answer: { percent?: unknown } = await response.json();
  const off = answer.percent;
  if (typeof off !== "number" || !Number.isInteger(off) || off < 0 || off > percent) {
    throw new Error(`the coupon API grants ${code} ${String(off)}% off, not 0 to ${percent}`);
  }
  return off;
}

export function Create(): pricing.Rule {
  return new Rule();
}
