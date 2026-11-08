// tspricing whose Coupon authenticates to the coupon service with the secret coupon_token, like gosecrets and
// luasecrets: the three run the same test cases (testdata/validate/secretcases). Its types are plg.d.ts's.

const hoard: string[] = [];

class Rule implements pricing.Rule {
  async Quote(order: pricing.Order) {
    const lines = order.lines ?? [];
    if (lines.length === 0) {
      throw new Error("empty order");
    }
    const total = lines.reduce((sum, line) => sum + line.qty * line.cents, 0);
    const quote = { total, discount: 0, reasons: [] as string[] };
    if ((await host.Tier(order.customer)) === "gold") {
      quote.discount = Math.floor(total / 10);
      quote.reasons.push("gold");
    }
    if (order.gift !== null) {
      quote.reasons.push("gift");
    }
    quote.total = total - quote.discount;
    return quote;
  }

  Split(cents: number, parts: number): [number[], number] {
    if (parts <= 0) {
      throw new Error("no parts");
    }
    return [new Array(parts).fill(Math.floor(cents / parts)), cents % parts];
  }

  async Coupon(code: string) {
    const url = "https://coupons.example.com/codes/" + code + "?token=" + secrets.get("coupon_token");
    const resp = await fetch(url);
    const body = await resp.text();
    if (resp.status === 200) {
      return parseInt(body, 10);
    }
    if (resp.status === 404) {
      throw new Error("unknown coupon " + code);
    }
    throw new Error("coupon service: " + resp.status);
  }

  Spin(mode: string) {
    if (mode === "panic") {
      hoard.push("x".repeat(1 << 27)); // twice the memory limit at once: out of memory without spinning
    }
    if (mode === "loop") {
      for (;;) {}
    }
  }

  Validate(limits: pricing.Limits): plg.Issue[] {
    if (limits.maxPercent < 10) {
      return [{
        severity: "error", code: "gold.percent", field: "maxPercent",
        message: `the gold discount (10%) is over the limit (${limits.maxPercent}%)`,
      }];
    }
    return [];
  }
}

export function Create(): pricing.Rule {
  return new Rule();
}
