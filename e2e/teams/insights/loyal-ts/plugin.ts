// An eligibility condition written in TypeScript: the insights team's loyalty rule. Gold members qualify with at least two orders in the last year,
// silver members with at least 150.00 of spend, and no one else; the reason says which, with the amounts. The
// team keeps the rule in Go, Lua and TypeScript (loyal-go, loyal-lua, loyal-ts), every port held to the same test
// cases, and the platform's tests to the same decisions over every user.
// Its types are plg.d.ts's (plg types writes them).

// The yearly orders a gold member needs, and the yearly spend a silver member needs.
const minGoldOrders = 2;
const minSilverCents = 15000;

// amount writes cents as a decimal amount: 15000 is 150.00.
function amount(cents: number): string {
  return `${Math.trunc(cents / 100)}.${String(cents % 100).padStart(2, "0")}`;
}

class Condition implements eligibility.Condition {
  Eligible(subject: eligibility.Subject): eligibility.Decision {
    const user = subject.user;
    if (user.tier === "gold") {
      if (user.orders < minGoldOrders) {
        return { eligible: false, reason: `gold member with ${user.orders} orders, under ${minGoldOrders}` };
      }
      return { eligible: true, reason: `gold member with ${user.orders} orders` };
    }
    if (user.tier === "silver") {
      if (user.spendCents < minSilverCents) {
        return {
          eligible: false,
          reason: `silver member who spent ${amount(user.spendCents)}, under ${amount(minSilverCents)}`,
        };
      }
      return { eligible: true, reason: `silver member who spent ${amount(user.spendCents)}` };
    }
    return { eligible: false, reason: `${user.tier} members are not in the loyalty programme` };
  }

  // Validate rejects the condition when the platform lets conditions require less spend than silver members need.
  Validate(limits: eligibility.Limits): plg.Issue[] {
    if (minSilverCents <= limits.maxSpendCents) {
      return [];
    }
    return [{
      severity: "error",
      code: "loyal.spend",
      message: `requires ${amount(minSilverCents)} of spend, over the limit of ${amount(limits.maxSpendCents)}`,
      field: "minSilverCents",
    }];
  }
}

export function Create(): eligibility.Condition {
  return new Condition();
}
