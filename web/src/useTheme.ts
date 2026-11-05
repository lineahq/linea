import { useTheme } from './ui/theme';

/** useDarkMode is true when the app shows the dark theme (preference or system). */
export function useDarkMode(): boolean {
  return useTheme() === 'dark';
}
