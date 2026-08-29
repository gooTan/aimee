/** @vitest-environment jsdom */
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import RoutingCanaryUiOnly from './RoutingCanaryUiOnly';

afterEach(() => {
  cleanup();
});

describe('RoutingCanaryUiOnly', () => {
  it('renders in the ready state initially', () => {
    render(<RoutingCanaryUiOnly />);
    const button = screen.getByRole('button', { name: 'UI routing canary ready' });
    expect(button.textContent).toBe('UI routing canary ready');
  });

  it('confirms on activation, updating visible text and accessible name', () => {
    render(<RoutingCanaryUiOnly />);
    fireEvent.click(screen.getByRole('button', { name: 'UI routing canary ready' }));

    const button = screen.getByRole('button', { name: 'UI routing canary confirmed' });
    expect(button.textContent).toBe('UI routing canary confirmed');
    expect(screen.queryByRole('button', { name: 'UI routing canary ready' })).toBeNull();
  });
});
