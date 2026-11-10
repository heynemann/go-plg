// A reward deliverer written in TypeScript: partner vouchers. A voucher's code is derived from the reward's
// idempotency key, so delivering a reward again yields the same voucher. Its types are plg.d.ts's (plg types
// writes them).
//
// It is broken on purpose: a reward of stuckFrom points or more never returns (it spins, as a deliverer stuck on
// its destination would wait), the hang the platform's end-to-end tests stop at their call timeout and attribute
// to this team.
const stuckFrom = 1000;

class Deliverer implements delivery.Deliverer {
  Deliver(reward: delivery.Reward): delivery.Receipt {
    while (reward.points >= stuckFrom) {
      // stuck
    }
    return { id: `voucher-${reward.idempotencyKey}`, duplicate: false };
  }
}

export function Create(): delivery.Deliverer {
  return new Deliverer();
}
