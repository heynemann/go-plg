// Gold says it implements cohort.Provider, but its List returns a page whose next cursor is a number.
class Gold implements cohort.Provider {
  async IsMember(user: string) {
    return (await host.Tier(user)) === "gold";
  }

  List(cursor: string) {
    return { users: [], next: 1 };
  }
}

export function Create() {
  return new Gold();
}
