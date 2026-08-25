/** @vitest-environment jsdom */
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import RoutingCanaryMixed from './RoutingCanaryMixed';
import { mixedCanaryMessage } from '../canaries/mixedContract';

afterEach(() => {
  cleanup();
});

describe('Mixed routing canary', () => {
  it('renders the message inside an accessible status region', () => {
    render(<RoutingCanaryMixed />);
    const region = screen.getByRole('status', { name: 'Mixed routing canary' });
    expect(region.textContent).toBe(mixedCanaryMessage);
    expect(screen.getByText(mixedCanaryMessage)).toBe(region);
  });
});
