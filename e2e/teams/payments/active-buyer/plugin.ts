// An eligibility condition written in TypeScript: users who placed at least three orders in the last year
// qualify. Its types are plg.d.ts's (plg types writes them).
const minimumOrders = 3;

class Condition implements eligibility.Condition {
  Eligible(subject: eligibility.Subject): eligibility.Decision {
    const orders = subject.user.orders;
    if (orders < minimumOrders) {
      return { eligible: false, reason: `${orders} orders, under ${minimumOrders}` };
    }
    return { eligible: true, reason: `${orders} orders` };
  }

  // Validate rejects the condition when the platform lets conditions require fewer orders than it does.
  Validate(limits: eligibility.Limits): plg.Issue[] {
    if (minimumOrders <= limits.maxOrders) {
      return [];
    }
    return [{
      severity: "error",
      code: "active-buyer.orders",
      message: `requires ${minimumOrders} orders, over the ${limits.maxOrders} orders limit`,
      field: "minimumOrders",
    }];
  }
}

export function Create(): eligibility.Condition {
  return new Condition();
}
