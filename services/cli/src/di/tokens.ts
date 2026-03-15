// DI tokens for tsyringe — Symbol-based for type safety

export const Service = {
  IpcClient: Symbol.for('Service.IpcClient'),
  Config: Symbol.for('Service.Config'),
  Logger: Symbol.for('Service.Logger'),
} as const;
