#!/usr/bin/env node
import { Text, render } from 'ink';
import React from 'react';

function App() {
  return <Text bold>boss</Text>;
}

render(<App />);
