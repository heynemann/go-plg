// A pricing rule written in TypeScript: the SUMMER coupon takes 8% off. Its types (pricing.Rule, pricing.Order,
// pricing.Limits, plg.Issue) are plg.d.ts's, which plg types writes from the contract.
const coupon = "SUMMER";
const percent = 8;

class Rule implements pricing.Rule {
  Discount(order: pricing.Order): pricing.Discount {
    if (order.coupon !== coupon) {
      return { cents: 0, reason: "" };
    }
    const subtotal = (order.items ?? []).reduce((sum, item) => sum + item.quantity * item.unitCents, 0);
    return { cents: Math.floor((subtotal * percent) / 100), reason: `coupon ${coupon}: ${percent}% off` };
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

export function Create(): pricing.Rule {
  return new Rule();
}
