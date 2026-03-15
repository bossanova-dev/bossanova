// DI tokens for tsyringe — Symbol-based for type safety

export const Service = {
  Database: Symbol.for('Service.Database'),
  RepoStore: Symbol.for('Service.RepoStore'),
  SessionStore: Symbol.for('Service.SessionStore'),
  AttemptStore: Symbol.for('Service.AttemptStore'),
  Config: Symbol.for('Service.Config'),
  Logger: Symbol.for('Service.Logger'),
  Dispatcher: Symbol.for('Service.Dispatcher'),
  IpcServer: Symbol.for('Service.IpcServer'),
  ClaudeSupervisor: Symbol.for('Service.ClaudeSupervisor'),
} as const;
