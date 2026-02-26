export interface User {
  id: string;
  name: string;
  email: string;
}

export function createUser(name: string, email: string): User {
  return {
    id: Math.random().toString(36).substring(7),
    name,
    email,
  };
}

export function validateEmail(email: string): boolean {
  const re = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
  return re.test(email);
}

export class UserService {
  private users: User[] = [];

  addUser(user: User): void {
    this.users.push(user);
  }

  findUser(id: string): User | undefined {
    return this.users.find((u) => u.id === id);
  }

  listUsers(): User[] {
    return [...this.users];
  }
}
