// IsMember calls a method the Host cohort.Users does not have.
export function Create(): cohort.Provider {
  return {
    IsMember: async (user: string) => (await host.Rank(user)) === "gold",
    List: (cursor: string) => ({ users: [], next: "" }),
  };
}
