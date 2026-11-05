import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { PageHead } from '../../ui/PageHead';
import { runtimesQuery, supportQuery } from '../RuntimeSelect';
import { runtimeRows } from './runtimeList';
import './runtimes.css';

/**
 * RuntimesHead is the shared header of the Runtimes destination: its title,
 * the section navigation (Runtimes, Upgrade check, Storage) with the number of
 * listed runtimes, and the page's own actions on the right.
 */
export function RuntimesHead({ actions }: { actions?: ReactNode }) {
  const runtimes = useQuery(runtimesQuery);
  const support = useQuery(supportQuery);
  const count = runtimeRows(runtimes.data, support.data?.built_in).length;
  return (
    <PageHead
      title="Runtimes"
      nav={[
        { to: '/runtimes', label: 'Runtimes', count, exact: true },
        { to: '/runtimes/upgrade', label: 'Upgrade check', exact: true },
        { to: '/runtimes/storage', label: 'Storage', exact: true },
      ]}
      actions={actions}
    />
  );
}
